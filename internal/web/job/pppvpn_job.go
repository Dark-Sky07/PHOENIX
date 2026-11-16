package job

import (
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

// PPPVPNJob reconciles local SSTP/PPTP daemons from the committed inbound and
// client records. PPP traffic accounting is not exposed by these server
// daemons, but authentication, process lifecycle, NAT and Xray routing are.
type PPPVPNJob struct {
	inboundService service.InboundService
	clientService  service.ClientService
	xrayService    service.XrayService
}

func NewPPPVPNJob() *PPPVPNJob { return &PPPVPNJob{} }

func (j *PPPVPNJob) Run() {
	inbounds, err := j.inboundService.GetAllInbounds()
	if err != nil {
		logger.Warning("pppvpn job: get inbounds failed:", err)
		return
	}
	var desired []pppvpn.Instance
	for _, ib := range inbounds {
		if ib == nil || !ib.Enable || ib.NodeID != nil || (ib.Protocol != model.SSTP && ib.Protocol != model.PPTP) {
			continue
		}
		clients, err := j.clientService.ListForInbound(nil, ib.Id)
		if err != nil {
			logger.Warningf("pppvpn job: list clients for inbound %d failed: %v", ib.Id, err)
			return
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
}
