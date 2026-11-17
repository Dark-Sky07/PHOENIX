package job

import (
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/websocket"
)

// PPPVPNJob reconciles local SSTP/PPTP daemons and collects their authenticated
// PPP interface counters into the shared client-traffic/quota pipeline.
type PPPVPNJob struct {
	runMu          sync.Mutex
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

func NewPPPVPNJob() *PPPVPNJob { return &PPPVPNJob{} }

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

func (j *PPPVPNJob) Run() {
	// The cron scheduler may overlap jobs, and startTask also performs an
	// immediate initial run. Keep checkpoint processing, file acknowledgement,
	// and checkpoint cleanup in one local critical section to avoid a replay
	// racing final-session cleanup.
	if !j.runMu.TryLock() {
		return
	}
	defer j.runMu.Unlock()
	if !j.reconcile() {
		return
	}
	batch, err := pppvpn.CollectTraffic()
	if err != nil {
		logger.Warning("pppvpn job: collect PPP traffic failed:", err)
		return
	}
	if batch == nil {
		return
	}
	needRestart := false
	clientsDisabled := false
	if len(batch.Clients) > 0 {
		needRestart, clientsDisabled, err = j.inboundService.AddTraffic(nil, batch.Clients)
		if err != nil {
			logger.Warning("pppvpn job: persist PPP traffic failed:", err)
			return // Keep the batch pending so the next poll retries without loss.
		}
	}
	if err := pppvpn.AckTraffic(batch.Token); err != nil {
		logger.Warning("pppvpn job: acknowledge PPP traffic failed:", err)
		return
	}
	if len(batch.ClosedSessionIDs) > 0 {
		if err := j.inboundService.DeletePPPTrafficCheckpoints(batch.ClosedSessionIDs); err != nil {
			logger.Warning("pppvpn job: delete completed traffic checkpoints failed:", err)
		}
	}
	if needRestart {
		j.xrayService.SetToNeedRestart()
	}
	if clientsDisabled {
		// Quota enforcement ran in AddTraffic. Reconcile again now rather than
		// waiting for the next cron interval, so a depleted PPP account loses
		// daemon credentials and any active session as promptly as possible.
		j.reconcile()

		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
}
