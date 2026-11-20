package job

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/outbound"
	"github.com/Dark-Sky07/phoenix/internal/web/websocket"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
)

// XrayTrafficJob collects and processes traffic statistics from Xray, updating the database and optionally informing external APIs.
type XrayTrafficJob struct {
	runMu           sync.Mutex
	pending         *xrayTrafficBatch
	captureUnlock   func()
	settingService  service.SettingService
	xrayService     service.XrayService
	inboundService  service.InboundService
	outboundService outbound.OutboundService
}

// NewXrayTrafficJob creates a new traffic collection job instance.
func NewXrayTrafficJob() *XrayTrafficJob {
	return new(XrayTrafficJob)
}

// The production scheduler can be replaced during an in-process panel
// restart. Keep its pending batch, capture lock and poll serialization with
// the panel process rather than abandoning them with the old scheduler.
// This is not durable across a process crash; the journal remains necessary.
var sharedXrayTrafficJob = NewXrayTrafficJob()

func SharedXrayTrafficJob() *XrayTrafficJob { return sharedXrayTrafficJob }

// Run collects traffic statistics from Xray, updates the database, and pushes
// real-time updates over WebSocket using compact delta payloads — no REST
// fallback, scales to 10k–20k+ clients per inbound.
func (j *XrayTrafficJob) Run() {
	j.runOnce(j.run)
}

// A poll and its database writes must remain ordered. Cron can invoke Run
// again while a slow poll is still persisting its previous delta; skip that
// tick rather than advance the baseline and persist a later interval first.
func (j *XrayTrafficJob) runOnce(run func()) {
	if !j.runMu.TryLock() {
		return
	}
	defer j.runMu.Unlock()
	run()
}

type xrayTrafficBatch struct {
	id         string
	traffic    []*xray.Traffic
	clients    []*xray.ClientTraffic
	identities []service.XrayTrafficIdentity
}

// applyPending keeps the same identity and counters on error, including an
// ambiguous commit. Call only while runMu is held.
func (j *XrayTrafficJob) applyPending(apply func(*xrayTrafficBatch) (bool, bool, error)) (bool, bool, error) {
	if j.pending == nil {
		return false, false, nil
	}
	restart, disabled, err := apply(j.pending)
	if err == nil {
		j.pending = nil
	}
	return restart, disabled, err
}

// Poll and identity capture must be ordered with participating client and
// reseller mutations. If capture fails after polling advanced the baseline,
// retain both the batch and lock until capture can be retried: releasing the
// lock earlier could attribute the delta to a replacement client or owner.
// This coordinates only paths that take the allocation lock; it is not a
// fencing mechanism for lost database sessions or an atomic runtime journal.
func (j *XrayTrafficJob) prepareXrayBatch(
	poll func() ([]*xray.Traffic, []*xray.ClientTraffic, error),
	capture func([]*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error),
) error {
	if j.pending == nil {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			return err
		}
		traffic, clients, err := poll()
		if err != nil {
			unlock()
			return err
		}
		j.pending = &xrayTrafficBatch{id: uuid.NewString(), traffic: traffic, clients: clients}
		j.captureUnlock = unlock
	}
	if j.pending.identities == nil {
		identities, err := capture(j.pending.clients)
		if err != nil {
			return fmt.Errorf("capture Xray traffic identity: %w", err)
		}
		if identities == nil {
			return fmt.Errorf("capture Xray traffic identity returned no snapshot")
		}
		j.pending.identities = identities
	}
	if j.captureUnlock != nil {
		j.captureUnlock()
		j.captureUnlock = nil
	}
	return nil
}

func (j *XrayTrafficJob) run() {
	// Never advance the in-memory baseline again while a previous batch is
	// uncommitted. The receipt makes an ambiguous commit safe to retry.
	if j.pending == nil {
		if err := j.replayXrayJournal(); err != nil {
			logger.Warning("recover Xray traffic journal failed:", err)
			return
		}
		if !j.xrayService.IsXrayRunning() {
			return
		}
	}
	if err := j.prepareXrayBatch(j.xrayService.GetXrayTraffic, j.inboundService.CaptureXrayTrafficIdentities); err != nil {
		logger.Warning("prepare Xray traffic batch failed; retaining capture lock when necessary:", err)
		return
	}
	batch := j.pending
	needRestart0, clientsDisabled, err := j.applyPending(func(b *xrayTrafficBatch) (bool, bool, error) {
		return j.applyJournaledXrayBatch(b)
	})
	if err != nil {
		if needRestart0 {
			j.xrayService.SetToNeedRestart()
		}
		logger.Warning("add inbound traffic failed; retaining batch for retry:", err)
		return
	}
	traffics, clientTraffics := batch.traffic, batch.clients
	err, needRestart1 := j.outboundService.AddTraffic(traffics, clientTraffics)
	if err != nil {
		logger.Warning("add outbound traffic failed:", err)
	}
	if clientsDisabled {
		restartOnDisable, settingErr := j.settingService.GetRestartXrayOnClientDisable()
		if settingErr != nil {
			logger.Warning("get RestartXrayOnClientDisable failed:", settingErr)
		}
		if restartOnDisable {
			if err := j.xrayService.RestartXray(true); err != nil {
				logger.Warning("restart xray after disabling clients failed:", err)
				j.xrayService.SetToNeedRestart()
			}
		}
		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
	if ExternalTrafficInformEnable, err := j.settingService.GetExternalTrafficInformEnable(); ExternalTrafficInformEnable {
		j.informTrafficToExternalAPI(traffics, clientTraffics)
	} else if err != nil {
		logger.Warning("get ExternalTrafficInformEnable failed:", err)
	}
	if needRestart0 || needRestart1 {
		j.xrayService.SetToNeedRestart()
	}

	// Derive the local online set from this poll's per-email deltas rather
	// than the shared last_online column, which remote-node syncs also bump
	// and would otherwise make a client active only on a remote node appear
	// online on local inbounds.
	activeEmails := make([]string, 0, len(clientTraffics))
	deltaActive := make(map[string]bool, len(clientTraffics))
	for _, ct := range clientTraffics {
		if ct != nil && ct.Up+ct.Down > 0 {
			activeEmails = append(activeEmails, ct.Email)
			deltaActive[ct.Email] = true
		}
	}
	// When the core supports the online-stats API, union in connection-based
	// onlines. Neither signal alone covers everything: an idle-but-connected
	// client moves no bytes between polls (the delta heuristic's blind spot),
	// while a short-lived connection can close before this poll yet still show
	// in the delta. Older cores fall back to deltas alone.
	if onlineUsers, apiMode, ouErr := j.xrayService.GetOnlineUsers(); ouErr != nil {
		logger.Debug("get online users from xray api failed:", ouErr)
	} else if apiMode {
		idleOnline := make([]string, 0, len(onlineUsers))
		for _, u := range onlineUsers {
			if !deltaActive[u.Email] {
				activeEmails = append(activeEmails, u.Email)
				idleOnline = append(idleOnline, u.Email)
			}
		}
		// The traffic path only bumps last_online on a non-zero delta; keep the
		// column fresh for clients kept online purely by a live connection.
		if err := j.inboundService.BumpClientsLastOnline(idleOnline); err != nil {
			logger.Warning("bump last online for connected clients failed:", err)
		}
	}
	// Pair the email signal with the inbound tags that moved bytes this poll.
	// Xray's user>>>email counter aggregates across every inbound a client is
	// attached to, so an online email alone can't say which inbound it used —
	// gating the per-inbound view on these tags keeps a multi-inbound client
	// off inbounds that saw no traffic. See issue #4859.
	activeInboundTags := make([]string, 0, len(traffics))
	for _, tr := range traffics {
		if tr != nil && tr.IsInbound && tr.Up+tr.Down > 0 {
			activeInboundTags = append(activeInboundTags, tr.Tag)
		}
	}
	j.inboundService.RefreshLocalOnlineClients(activeEmails, activeInboundTags)

	if !websocket.HasClients() {
		return
	}

	lastOnlineMap, err := j.inboundService.GetClientsLastOnline()
	if err != nil {
		logger.Warning("get clients last online failed:", err)
	}
	if lastOnlineMap == nil {
		lastOnlineMap = make(map[string]int64)
	}
	onlineClients := j.inboundService.GetOnlineClients()
	if onlineClients == nil {
		onlineClients = []string{}
	}
	websocket.BroadcastTraffic(map[string]any{
		"traffics":       traffics,
		"clientTraffics": clientTraffics,
		"onlineClients":  onlineClients,
		"onlineByGuid":   j.inboundService.GetOnlineClientsByGuid(),
		"activeInbounds": j.inboundService.GetActiveInboundsByGuid(),
		"lastOnlineMap":  lastOnlineMap,
	})

	clientStatsPayload := map[string]any{}
	if stats, err := j.inboundService.GetAllClientTraffics(); err != nil {
		logger.Warning("get all client traffics for websocket failed:", err)
	} else if len(stats) > 0 {
		clientStatsPayload["clients"] = stats
	}
	if inboundSummary, err := j.inboundService.GetInboundsTrafficSummary(); err != nil {
		logger.Warning("get inbounds traffic summary for websocket failed:", err)
	} else if len(inboundSummary) > 0 {
		clientStatsPayload["inbounds"] = inboundSummary
	}
	if len(clientStatsPayload) > 0 {
		websocket.BroadcastClientStats(clientStatsPayload)
	}

	if updatedOutbounds, err := j.outboundService.GetOutboundsTraffic(); err == nil && updatedOutbounds != nil {
		websocket.BroadcastOutbounds(updatedOutbounds)
	} else if err != nil {
		logger.Warning("get all outbounds for websocket failed:", err)
	}
}

func (j *XrayTrafficJob) informTrafficToExternalAPI(inboundTraffics []*xray.Traffic, clientTraffics []*xray.ClientTraffic) {
	informURL, err := j.settingService.GetExternalTrafficInformURI()
	if err != nil {
		logger.Warning("get ExternalTrafficInformURI failed:", err)
		return
	}
	informURL, err = service.SanitizePublicHTTPURL(informURL, false)
	if err != nil {
		logger.Warning("ExternalTrafficInformURI blocked:", err)
		return
	}
	requestBody, err := json.Marshal(map[string]any{"clientTraffics": clientTraffics, "inboundTraffics": inboundTraffics})
	if err != nil {
		logger.Warning("parse client/inbound traffic failed:", err)
		return
	}
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	request.Header.SetMethod("POST")
	request.Header.SetContentType("application/json; charset=UTF-8")
	request.SetBody([]byte(requestBody))
	request.SetRequestURI(informURL)
	response := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(response)
	if err := fasthttp.Do(request, response); err != nil {
		logger.Warning("POST ExternalTrafficInformURI failed:", err)
	}
}
