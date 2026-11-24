package job

import (
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/openvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// OpenvpnJob reconciles the running openvpn daemon processes against the
// enabled openvpn inbounds in the database, restarts any that crashed or
// changed, and folds the per-client traffic read from each daemon's
// management interface into the usual inbound/client traffic accounting.
// It mirrors MtprotoJob: the daemons live entirely outside the Xray config.
type OpenvpnJob struct {
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

// NewOpenvpnJob creates a new openvpn reconcile/traffic job instance.
// Zero-value services are fine: like the other jobs, this one resolves
// state through the global database handle inside the service methods.
func NewOpenvpnJob() *OpenvpnJob {
	return new(OpenvpnJob)
}

// enabledClientsForInbound returns the emails of the clients attached to the
// inbound that are actually allowed to connect — mirroring the exact
// enable-logic of the Xray config renderer (client record Enable plus the
// shared client-traffic row, where the auto-disable job records quota and
// expiry violations). Pruning local certificate files does not revoke an
// already-issued profile; reconnect and active sessions remain unproven.
func (j *OpenvpnJob) enabledClientsForInbound(ib *model.Inbound) ([]string, bool) {
	dbClients, err := j.clientService.ListForInbound(nil, ib.Id)
	if err != nil {
		logger.Warning("openvpn job: list clients for inbound", ib.Id, "failed:", err)
		// A lookup failure is not the same as an inbound with no clients. Keep
		// the current daemon configuration intact until the next healthy poll.
		return nil, false
	}
	clients, err := j.inboundService.FilterDaemonAccountingClients(dbClients)
	if err != nil {
		logger.Warning("openvpn job: filter accounting identities failed:", err)
		return nil, false
	}
	emails := make([]string, 0, len(clients))
	for _, client := range clients {
		emails = append(emails, client.Email)
	}
	return emails, true
}

// The OpenVPN manager and its counters are process-wide. Retain one pending
// accounting batch across scheduler replacements and serialize poll/reconcile.
var sharedOpenvpnAccounting = newOpenvpnAccounting()

func newOpenvpnAccounting() *XrayTrafficJob {
	accounting := NewXrayTrafficJob()
	accounting.manualPoll = true
	return accounting
}

// commitOpenvpnPoll reuses the Xray journal, shared allocation fence and
// payload-bound receipt. A crash leaves a non-replayable manual journal.
// The caller holds accounting.runMu. Once a poll has
// advanced in-memory baselines, a failed write retries exactly that batch and
// must not poll or reconcile again. An uncommitted crash marker is not proof
// of a complete poll and continues to require manual reconciliation.
func commitOpenvpnPoll(accounting *XrayTrafficJob, poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error)) error {
	if accounting.pending == nil {
		if err := accounting.replayXrayJournal(); err != nil {
			return fmt.Errorf("recover traffic journal: %w", err)
		}
	}
	if err := accounting.prepareXrayBatch(poll, accounting.inboundService.CaptureXrayTrafficIdentities); err != nil {
		return fmt.Errorf("prepare OpenVPN traffic: %w", err)
	}
	restart, _, err := accounting.applyPending(accounting.applyJournaledXrayBatch)
	if restart {
		accounting.xrayService.SetToNeedRestart()
	}
	if err != nil {
		return fmt.Errorf("commit OpenVPN traffic: %w", err)
	}
	return nil
}

func collectOpenvpnTraffic(mgr *openvpn.Manager) ([]*xray.Traffic, []*xray.ClientTraffic, error) {
	inboundDeltas, clientDeltas, err := mgr.CollectTrafficWithErrors()
	traffics := make([]*xray.Traffic, 0, len(inboundDeltas))
	for _, d := range inboundDeltas {
		traffics = append(traffics, &xray.Traffic{IsInbound: true, Tag: d.Tag, Up: d.Up, Down: d.Down})
	}
	clients := make([]*xray.ClientTraffic, 0, len(clientDeltas))
	for _, d := range clientDeltas {
		clients = append(clients, &xray.ClientTraffic{InboundId: d.InboundId, Email: d.Email, Enable: true, Up: d.Up, Down: d.Down})
	}
	return traffics, clients, err
}

// Run records the observed counters before potentially restarting daemons.
// The poll is not a drain: traffic may still arrive before or during restart.
func (j *OpenvpnJob) Run() {
	accounting := sharedOpenvpnAccounting
	if !accounting.runMu.TryLock() {
		return
	}
	defer accounting.runMu.Unlock()
	mgr := openvpn.GetManager()
	hadPending := accounting.pending != nil
	var pollErr error
	if err := commitOpenvpnPoll(accounting, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		traffic, clients, err := collectOpenvpnTraffic(mgr)
		pollErr = err
		// Some other daemons may have advanced their baselines even when one
		// failed. Commit those samples before reporting the partial failure.
		return traffic, clients, nil
	}); err != nil {
		logger.Warning("openvpn job: traffic not committed; retaining pending evidence:", err)
		return
	}
	if hadPending || pollErr != nil {
		if pollErr != nil {
			logger.Warning("openvpn job: partial poll; delaying daemon reconciliation:", pollErr)
		}
		return
	}

	inbounds, err := j.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("openvpn job: get inbounds failed:", err)
		return
	}
	desired, err := buildOpenvpnDesired(inbounds, j.enabledClientsForInbound, j.xrayService.DaemonRelayPort)
	if err != nil {
		logger.Warning("openvpn job: delaying reconciliation:", err)
		return
	}
	mgr.Reconcile(desired)
}

// Build the complete desired set or reject it; a partial list would make
// reconciliation stop otherwise healthy daemons omitted after an error.
func buildOpenvpnDesired(inbounds []*model.Inbound, clientsFor func(*model.Inbound) ([]string, bool), relayPort func(string) int) ([]openvpn.Instance, error) {
	var desired []openvpn.Instance
	for _, ib := range inbounds {
		if ib == nil {
			return nil, fmt.Errorf("nil inbound in snapshot")
		}
		if ib.Protocol != model.OpenVPN || !ib.Enable || ib.NodeID != nil {
			continue
		}
		clients, ok := clientsFor(ib)
		if !ok {
			return nil, fmt.Errorf("inbound %d client lookup failed", ib.Id)
		}
		inst, ok := openvpn.InstanceFromInbound(ib, clients)
		if !ok {
			return nil, fmt.Errorf("invalid inbound %d", ib.Id)
		}
		inst.XrayRelayPort = relayPort(ib.Tag)
		desired = append(desired, inst)
	}
	return desired, nil
}
