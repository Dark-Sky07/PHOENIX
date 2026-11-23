package job

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// L2TPJob reconciles the single local L2TP/IPsec daemon group and its runtime
// FORWARD/MASQUERADE rules. PPP interface counters are folded into the normal
// inbound/client traffic pipeline; no synthetic Xray counters are generated.
type L2TPJob struct {
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

func NewL2TPJob() *L2TPJob { return new(L2TPJob) }

func (j *L2TPJob) enabledClientsForInbound(ib *model.Inbound) ([]model.Client, bool) {
	clients, err := j.clientService.ListForInbound(nil, ib.Id)
	if err != nil {
		logger.Warning("l2tp job: list clients failed:", err)
		// A database error must not make the daemon fall back to credentials
		// copied into inbound.settings from an older reconcile round, nor
		// should it make Reconcile([]) tear down a healthy current daemon.
		return nil, false
	}
	filtered, err := j.inboundService.FilterDaemonAccountingClients(clients)
	if err != nil {
		logger.Warning("l2tp job: filter accounting identities failed:", err)
		return nil, false
	}
	return filtered, true
}

// Multiple scheduler instances and dashboard actions share one collector.
// Keep any uncommitted sample and allocation lock across all of them.
var l2tpRunMu sync.Mutex
var l2tpJournalCollector = NewXrayTrafficJob()
var errEmptyL2TPSample = errors.New("no observed L2TP traffic")

// persistL2TPPoll uses the same identity-checked journal, durable in-flight
// marker and transactional receipt as Xray. It covers only sampled bytes: the
// L2TP runtime can still lose traffic before a sample or during shutdown.
func persistL2TPPoll(collector *XrayTrafficJob, poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error)) (bool, error) {
	if collector.pending == nil && collector.l2tpHookPollID == "" {
		if err := collector.replayXrayJournal(); err != nil {
			return false, err
		}
	}
	if err := collector.prepareXrayBatch(poll, collector.inboundService.CaptureXrayTrafficIdentities); err != nil {
		if errors.Is(err, errEmptyL2TPSample) {
			return false, nil
		}
		return false, err
	}
	restart, disabled, err := collector.applyPending(collector.applyJournaledXrayBatch)
	return restart || disabled, err
}

// pollL2TPWithIncidentBarrier still samples after an incident publication
// error so that its poll marker can fence an observed hook or orphan. It never
// authorizes daemon reconciliation while the publication is unresolved.
func pollL2TPWithIncidentBarrier(collector *XrayTrafficJob, publish func() error, poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error)) (bool, error) {
	var publicationErr error
	// An unapplied batch can also retain the allocation lock across ticks.
	// Publishing first would recursively acquire that same lock on an incident.
	skippedPublication := collector.l2tpRestorePoll || collector.captureUnlock != nil
	if !skippedPublication {
		publicationErr = publish()
	}
	restart, err := persistL2TPPoll(collector, poll)
	if err != nil {
		return restart, err
	}
	if skippedPublication || publicationErr == nil {
		// Marker restoration may hold the allocation lock; only publish after
		// a successful poll has restored the fence and released that lock.
		// Otherwise recheck for incidents observed during the poll. Hooks are
		// not synchronized with this check, so it is not a quiescence proof.
		publicationErr = publish()
	}
	if publicationErr != nil {
		return restart, fmt.Errorf("L2TP incident publication unresolved; reconciliation deferred: %w", publicationErr)
	}
	return restart, nil
}

func (j *L2TPJob) Run() {
	if !l2tpRunMu.TryLock() {
		return
	}
	defer l2tpRunMu.Unlock()
	mgr := l2tp.GetManager()
	// Sample and commit before the periodic reconcile may stop or replace the
	// daemon. Other runtime operations and traffic after the poll remain outside
	// this barrier; a successful sample is not a final accounting proof.
	restart, err := pollL2TPWithIncidentBarrier(l2tpJournalCollector, service.PublishL2TPAccountingIncident, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		inboundDeltas, clientDeltas, err := mgr.CollectTrafficChecked()
		if err != nil {
			return nil, nil, err
		}
		if len(inboundDeltas) == 0 && len(clientDeltas) == 0 {
			// No observed delta to apply. Clear the in-flight marker instead of
			// retaining an empty receipt every tick; this does not establish
			// that closed sessions had no unobserved traffic.
			return nil, nil, errEmptyL2TPSample
		}
		traffics := make([]*xray.Traffic, 0, len(inboundDeltas))
		for _, delta := range inboundDeltas {
			traffics = append(traffics, &xray.Traffic{IsInbound: true, Tag: delta.Tag, Up: delta.Up, Down: delta.Down})
		}
		clients := make([]*xray.ClientTraffic, 0, len(clientDeltas))
		for _, delta := range clientDeltas {
			clients = append(clients, &xray.ClientTraffic{InboundId: delta.InboundId, Email: delta.Email, Enable: true, Up: delta.Up, Down: delta.Down})
		}
		return traffics, clients, nil
	})
	if restart {
		j.xrayService.SetToNeedRestart()
	}
	if err != nil {
		logger.Warning("l2tp job: poll or accounting barrier failed; reconciliation deferred:", err)
		return
	}
	inbounds, err := j.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("l2tp job: get inbounds failed:", err)
		return
	}
	wanted := make([]l2tp.Instance, 0, 1)
	for _, ib := range inbounds {
		if ib.Protocol != model.L2TP || !ib.Enable || ib.NodeID != nil {
			continue
		}
		clients, clientsOK := j.enabledClientsForInbound(ib)
		if !clientsOK {
			return
		}
		inst, ok := l2tp.InstanceFromInbound(ib, clients)
		if !ok {
			logger.Warningf("l2tp job: inbound %d has invalid settings or credentials", ib.Id)
			continue
		}
		// See OpenvpnJob: follow the running core's relay listener each
		// round; excluded from the daemon fingerprint so it never restarts
		// the daemons.
		inst.XrayRelayPort = j.xrayService.DaemonRelayPort(ib.Tag)
		wanted = append(wanted, inst)
	}
	mgr.Reconcile(wanted)
}
