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
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

func NewPPPVPNJob() *PPPVPNJob { return &PPPVPNJob{} }

// All job instances share the same process-local spool collector and token.
var pppVPNRunMu sync.Mutex

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
	// Keep committed session checkpoints even after removing the closed spool
	// file: another process may already have captured the same final record.
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
