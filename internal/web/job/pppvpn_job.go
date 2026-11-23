package job

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/websocket"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// PPPVPNJob reconciles local SSTP/PPTP daemons and collects their authenticated
// PPP interface counters into the shared client-traffic/quota pipeline.
type PPPVPNJob struct {
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

func NewPPPVPNJob() *PPPVPNJob { return &PPPVPNJob{} }

// All job instances share the same process-local spool collector and token.
var pppVPNRunMu sync.Mutex
var sharedPPPPoll pppPollState

type pppPollState struct {
	unlock          func()
	marker          string
	deleteUncertain bool
}

// collectAndCommitPPP keeps a durable fence and the exact pending spool batch
// on failure. The allocation lock is released between ordinary retries so
// independent journal recovery can proceed, but normal mutations remain fenced.
// An ambiguous marker deletion keeps the lock because it might have succeeded.
func collectAndCommitPPP(state *pppPollState, collect func() (*pppvpn.TrafficBatch, error), apply func(string, []*xray.ClientTraffic) (bool, bool, error), ack func(uint64) error) (bool, bool, error) {
	if state.unlock == nil {
		var unlock func()
		var err error
		if state.marker == "" {
			unlock, err = service.LockResellerAllocation()
		} else {
			unlock, err = service.LockResellerAllocationForXrayRecovery()
		}
		if err != nil {
			return false, false, err
		}
		state.unlock = unlock
		if state.marker == "" {
			id := uuid.NewString()
			if err := service.BeginXrayPoll(id); err != nil {
				state.unlock()
				state.unlock = nil
				return false, false, fmt.Errorf("create PPP poll marker: %w", err)
			}
			state.marker = id
		} else if err := service.VerifyXrayPollMarker(state.marker); err != nil {
			state.unlock()
			state.unlock = nil
			return false, false, fmt.Errorf("verify PPP poll marker: %w", err)
		}
	}
	if state.deleteUncertain {
		// The delete may have committed despite returning an error. Do not
		// release the lock until the durable fence has been restored.
		if err := service.EnsureXrayPollMarker(state.marker); err != nil {
			return false, false, fmt.Errorf("restore uncertain PPP poll marker: %w", err)
		}
		state.deleteUncertain = false
	}
	release := func() { state.unlock(); state.unlock = nil }
	batch, err := collect()
	if err != nil {
		release()
		return false, false, fmt.Errorf("collect PPP traffic; poll marker %s retained: %w", state.marker, err)
	}
	if batch == nil {
		release()
		return false, false, fmt.Errorf("missing PPP batch; poll marker %s retained", state.marker)
	}
	var restart, disabled bool
	if len(batch.Clients) > 0 {
		restart, disabled, err = apply(state.marker, batch.Clients)
		if err != nil {
			var decrease *service.PPPCounterDecrease
			if errors.As(err, &decrease) && service.ConfirmPPPCounterIncidentUnderAllocationLock(decrease) == nil {
				if clearErr := service.FinishXrayPoll(state.marker); clearErr != nil {
					state.deleteUncertain = true
					return restart, disabled, fmt.Errorf("PPP incident: %v; clear poll marker: %w", err, clearErr)
				}
				state.marker = ""
			}
			release()
			return restart, disabled, err
		}
	}
	// A hook can replace a closed record between collection and acknowledgement.
	// Preserve the fence until cleanup confirms that no changed sample was lost.
	if err := ack(batch.Token); err != nil {
		release()
		return restart, disabled, fmt.Errorf("acknowledge PPP traffic; poll marker %s retained: %w", state.marker, err)
	}
	if err := service.FinishXrayPoll(state.marker); err != nil {
		state.deleteUncertain = true
		return restart, disabled, fmt.Errorf("clear committed PPP poll marker: %w", err)
	}
	state.marker = ""
	release()
	return restart, disabled, nil
}

func (j *PPPVPNJob) reconcile() bool {
	inbounds, err := j.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("pppvpn job: get inbounds failed:", err)
		return false
	}
	var desired []pppvpn.Instance
	for _, ib := range inbounds {
		if ib == nil || !ib.Enable || ib.NodeID != nil || (ib.Protocol != model.SSTP && ib.Protocol != model.PPTP) {
			continue
		}
		clients, err := j.clientService.ListForInbound(nil, ib.Id)
		if err != nil {
			logger.Warningf("pppvpn job: list clients for inbound %d failed: %v", ib.Id, err)
			return false
		}
		disabled := make(map[string]bool, len(ib.ClientStats))
		for _, stat := range ib.ClientStats {
			disabled[strings.ToLower(strings.TrimSpace(stat.Email))] = !stat.Enable
		}
		active := make([]model.Client, 0, len(clients))
		for _, client := range clients {
			if !client.Enable || disabled[strings.ToLower(strings.TrimSpace(client.Email))] {
				continue
			}
			active = append(active, client)
		}
		inst, ok := pppvpn.InstanceFromInbound(ib, active)
		if !ok {
			logger.Warningf("pppvpn job: inbound %d has invalid settings or credentials", ib.Id)
			continue
		}
		inst.XrayRelayPort = j.xrayService.DaemonRelayPort(ib.Tag)
		desired = append(desired, inst)
	}
	pppvpn.GetManager().Reconcile(desired)
	return true
}

func (j *PPPVPNJob) Run() { j.runOnce(j.run) }

func (j *PPPVPNJob) runOnce(run func()) {
	if !pppVPNRunMu.TryLock() {
		return
	}
	defer pppVPNRunMu.Unlock()
	run()
}

func (j *PPPVPNJob) run() {
	// Capture before periodic reconciliation can replace the daemon. Other
	// runtime paths and unobserved/final traffic remain outside this barrier.
	needRestart, clientsDisabled, err := collectAndCommitPPP(&sharedPPPPoll, pppvpn.CollectTraffic, j.inboundService.AddPPPTrafficUnderAllocationLock, pppvpn.AckTraffic)
	if needRestart {
		j.xrayService.SetToNeedRestart()
	}
	if err != nil {
		logger.Warning("pppvpn job: PPP collection or persistence failed:", err)
		return
	}
	j.reconcile()
	if clientsDisabled {
		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
}
