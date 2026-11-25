package job

import (
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/openvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"github.com/google/uuid"
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
func commitOpenvpnPoll(accounting *XrayTrafficJob, poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error)) (pollErr, commitErr error) {
	if accounting.pending == nil {
		if err := accounting.replayXrayJournal(); err != nil {
			return nil, fmt.Errorf("recover traffic journal: %w", err)
		}
	}
	// Even when a daemon poll fails, another daemon may have already
	// advanced its baseline. Always journal those deltas first; expose the
	// partial error separately so callers cannot mistake it for completeness.
	partialPoll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		traffic, clients, err := poll()
		pollErr = err
		return traffic, clients, nil
	}
	if err := accounting.prepareXrayBatch(partialPoll, accounting.inboundService.CaptureXrayTrafficIdentities); err != nil {
		return pollErr, fmt.Errorf("prepare OpenVPN traffic: %w", err)
	}
	restart, _, err := accounting.applyPending(accounting.applyJournaledXrayBatch)
	if restart {
		accounting.xrayService.SetToNeedRestart()
	}
	if err != nil {
		return pollErr, fmt.Errorf("commit OpenVPN traffic: %w", err)
	}
	return pollErr, nil
}

// Reflect accounting failures independently of daemon poll errors. The
// dashboard receives only a generic warning; detailed errors remain in logs.
func commitOpenvpnPollAndReport(accounting *XrayTrafficJob, mgr *openvpn.Manager, poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error)) (error, error) {
	pollErr, commitErr := commitOpenvpnPoll(accounting, poll)
	mgr.SetAccountingResult(commitErr)
	return pollErr, commitErr
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

// Closed-session evidence is committed before management polling. Its marker
// deliberately spans a retry: if the transaction or marker deletion is
// uncertain, other allocation paths stay fenced until a later tick proves the
// outcome from the database checkpoint and the durable session files.
type openvpnSessionPollState struct {
	marker string
	unlock func()
}

var sharedOpenvpnSessionPoll openvpnSessionPollState

func (j *OpenvpnJob) commitSessionEvidence() error {
	if database.GetDB() == nil {
		// Unit tests without a database still exercise daemon polling; the
		// production scheduler always has one.
		return nil
	}
	samples, err := openvpn.CollectSessionEvidence()
	if err != nil {
		return fmt.Errorf("inspect OpenVPN session evidence: %w", err)
	}
	state := &sharedOpenvpnSessionPoll
	if len(samples) == 0 && state.unlock == nil {
		return nil
	}
	if state.unlock == nil {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			return fmt.Errorf("lock allocation for OpenVPN sessions: %w", err)
		}
		id := uuid.NewString()
		if err := service.BeginXrayPoll(id); err != nil {
			unlock()
			return fmt.Errorf("create OpenVPN session poll marker: %w", err)
		}
		state.marker, state.unlock = id, unlock
	} else if err := service.VerifyXrayPollMarker(state.marker); err != nil {
		return fmt.Errorf("verify OpenVPN session poll marker: %w", err)
	}
	if len(samples) > 0 {
		clients := make([]*xray.ClientTraffic, 0, len(samples))
		for _, sample := range samples {
			clients = append(clients, &xray.ClientTraffic{
				Email: sample.Email, Enable: true,
				OpenVPNSessionID: sample.Key, OpenVPNInboundID: sample.InboundID,
				OpenVPNRxBytes: sample.Rx, OpenVPNTxBytes: sample.Tx,
			})
		}
		if _, _, err := j.inboundService.AddOpenVPNSessionTrafficUnderAllocationLock(state.marker, clients); err != nil {
			return fmt.Errorf("commit OpenVPN session usage: %w", err)
		}
		// A failed acknowledgement is safe to retry: the checkpoint makes a
		// replay a zero-delta operation. It must not leave a permanent fence.
		for _, sample := range samples {
			if err := openvpn.AckSessionEvidence(sample); err != nil {
				logger.Warning("openvpn: retain acknowledged session evidence for retry:", err)
			}
		}
	}
	if err := service.FinishXrayPoll(state.marker); err != nil {
		// The deletion may have committed despite the error. Keep the fence
		// and lock until the marker is demonstrably restored.
		if restoreErr := service.EnsureXrayPollMarker(state.marker); restoreErr != nil {
			logger.Warning("openvpn: restore uncertain session poll marker failed:", restoreErr)
		}
		return fmt.Errorf("finish OpenVPN session poll marker: %w", err)
	}
	state.unlock()
	state.unlock = nil
	state.marker = ""
	return nil
}

// Run records the observed counters before potentially restarting daemons.
// The poll is not a drain: traffic may still arrive before or during restart.
func (j *OpenvpnJob) Run() {
	accounting := sharedOpenvpnAccounting
	if !accounting.runMu.TryLock() {
		return
	}
	defer accounting.runMu.Unlock()
	if err := j.commitSessionEvidence(); err != nil {
		logger.Warning("openvpn: session evidence pending; delaying polling and reconciliation:", err)
		return
	}
	mgr := openvpn.GetManager()
	hadPending := accounting.pending != nil
	pollErr, err := commitOpenvpnPollAndReport(accounting, mgr, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return collectOpenvpnTraffic(mgr)
	})
	if err != nil {
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
