package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/amneziawg"
	"github.com/Dark-Sky07/phoenix/internal/amneziawgnet"
	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/util/json_util"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"go.uber.org/atomic"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	p                          *xray.Process
	lock                       sync.Mutex
	isNeedXrayRestart          atomic.Bool // Indicates that restart was requested for Xray
	isManuallyStopped          atomic.Bool // Indicates that Xray was stopped manually from the panel
	xrayCounterIncidentPending atomic.Bool // Local in-memory barrier until persistence succeeds.
	xrayCoreExitPending        atomic.Bool // An unexpected core exit is not yet durable in the database.
	xrayAPIOutagePending       atomic.Bool // An unreachable local API is not yet durable in the database.
	result                     string
	// One baseline per panel process, shared by every XrayService instance.
	// The state is protected by lock, which also protects process changes.
	trafficState xrayTrafficState
)

// Reaped cores are registered before the watcher waits for the process lock.
// Quota/removal checks can therefore fail closed while that lock is busy.
var pendingXrayExitObservations = struct {
	sync.Mutex
	processes map[*xray.Process]struct{}
}{processes: make(map[*xray.Process]struct{})}

func rememberXrayExitObservation(process *xray.Process) {
	pendingXrayExitObservations.Lock()
	pendingXrayExitObservations.processes[process] = struct{}{}
	pendingXrayExitObservations.Unlock()
}

func hasPendingXrayExitObservation() bool {
	pendingXrayExitObservations.Lock()
	defer pendingXrayExitObservations.Unlock()
	return len(pendingXrayExitObservations.processes) > 0
}

type xrayTrafficState struct {
	api                    xray.XrayAPI
	process                *xray.Process
	pendingCounterDecrease *xray.UserTrafficCounterDecrease
	pendingCounterMissing  *xray.UserTrafficCounterMissing
}

// observeProcess is called under lock. The only assignment to p starts a new
// managed core; it never attaches to an existing process, so its counters begin
// at zero. Sharing this state prevents a replacement job from counting the
// same core's cumulative counters twice.
func (t *xrayTrafficState) observeProcess(current *xray.Process) {
	if t.process != current {
		t.api.ResetTrafficBaseline(true)
		t.process = current
	}
}

// XrayService provides business logic for Xray process management.
// It handles starting, stopping, restarting Xray, and managing its configuration.
type XrayService struct {
	inboundService InboundService
	settingService SettingService
	xrayAPI        xray.XrayAPI
}

// IsXrayRunning checks if the Xray process is currently running.
func (s *XrayService) IsXrayRunning() bool {
	return p != nil && p.IsRunning()
}

// XrayStartedAt returns the current managed-core epoch. A process can be alive
// while it is still loading a large generated configuration and has not bound
// its API listener yet; health monitoring uses this timestamp to avoid treating
// that normal startup interval as an API outage.
func (s *XrayService) XrayStartedAt() time.Time {
	lock.Lock()
	defer lock.Unlock()
	if p == nil || !p.IsRunning() {
		return time.Time{}
	}
	return p.StartedAt()
}

// XrayProcess returns the current Xray process instance (may be nil when Xray
// is not running). It exposes the package-level process to callers outside this
// package (e.g. the tgbot subpackage) without changing access semantics.
func XrayProcess() *xray.Process {
	return p
}

// GetXrayErr returns the error from the Xray process, if any.
func (s *XrayService) GetXrayErr() error {
	if p == nil {
		return nil
	}

	err := p.GetErr()
	if err == nil {
		return nil
	}

	if runtime.GOOS == "windows" && err.Error() == "exit status 1" {
		// exit status 1 on Windows means that Xray process was killed
		// as we kill process to stop in on Windows, this is not an error
		return nil
	}

	return err
}

// GetXrayResult returns the result string from the Xray process.
func (s *XrayService) GetXrayResult() string {
	if result != "" {
		return result
	}
	if s.IsXrayRunning() {
		return ""
	}
	if p == nil {
		return ""
	}

	result = p.GetResult()

	if runtime.GOOS == "windows" && result == "exit status 1" {
		// exit status 1 on Windows means that Xray process was killed
		// as we kill process to stop in on Windows, this is not an error
		return ""
	}

	return result
}

// GetXrayVersion returns the version of the running Xray process.
func (s *XrayService) GetXrayVersion() string {
	if p == nil {
		return "Unknown"
	}
	return p.GetVersion()
}

// RemoveIndex removes an element at the specified index from a slice.
// Returns a new slice with the element removed.
func RemoveIndex(s []any, index int) []any {
	return append(s[:index], s[index+1:]...)
}

// GetXrayConfig retrieves and builds the Xray configuration from settings and inbounds.
func (s *XrayService) GetXrayConfig() (*xray.Config, error) {
	templateConfig, err := s.settingService.GetXrayConfigTemplate()
	if err != nil {
		return nil, err
	}

	xrayConfig := &xray.Config{}
	err = json.Unmarshal([]byte(templateConfig), xrayConfig)
	if err != nil {
		return nil, err
	}
	xrayConfig.LogConfig = resolveXrayLogPaths(xrayConfig.LogConfig)
	xrayConfig.API = ensureAPIServices(xrayConfig.API)
	xrayConfig.Policy = ensureStatsPolicy(xrayConfig.Policy)

	// Config generation runs on Xray startup and restart as well as ordinary
	// admin reads. During a transactional panel replacement the updater keeps a
	// durable marker until the replacement runtime is stable; traffic counters
	// are intentionally unavailable in that interval. Do not turn this read-like
	// path into renewal, expiry, or quota enforcement while that marker exists:
	// doing so can persistently disable otherwise valid users before the first
	// safe traffic poll. The configuration still reflects the existing durable
	// enable state and is therefore safe to generate for the replacement core.
	if !UpdateMaintenanceActive() {
		_, _, _ = s.inboundService.AddTraffic(nil, nil)
	}

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	for _, inbound := range inbounds {
		if !inbound.Enable {
			continue
		}
		if inbound.NodeID != nil {
			continue
		}
		if inbound.Protocol == model.MTProto || inbound.Protocol == model.AmneziaWG || inbound.Protocol == model.TUIC {
			continue
		}
		// OpenVPN, L2TP/IPsec, SSTP, and PPTP use host daemons that own
		// their sockets; these protocols must never appear in Xray config.
		if inbound.Protocol == model.OpenVPN || inbound.Protocol == model.L2TP || inbound.Protocol == model.SSTP || inbound.Protocol == model.PPTP {
			// Host-daemon listeners and credentials are reconciled separately.
			continue
		}
		settings := map[string]any{}
		json.Unmarshal([]byte(inbound.Settings), &settings)

		dbClients, listErr := s.inboundService.clientService.ListForInbound(nil, inbound.Id)
		if listErr != nil {
			return nil, listErr
		}

		clientStats := inbound.ClientStats
		enableMap := make(map[string]bool, len(clientStats))
		for _, clientTraffic := range clientStats {
			enableMap[transferEmailKey(clientTraffic.Email)] = clientTraffic.Enable
		}

		var finalClients []any
		for i := range dbClients {
			c := dbClients[i]
			if enable, exists := enableMap[transferEmailKey(c.Email)]; exists && !enable {
				logger.Infof("Remove Inbound User %s due to expiration or traffic limit", c.Email)
				continue
			}
			if !c.Enable {
				continue
			}
			flow := c.Flow
			if flow == "xtls-rprx-vision-udp443" {
				flow = "xtls-rprx-vision"
			}
			entry := map[string]any{"email": c.Email}
			switch inbound.Protocol {
			case model.VLESS:
				if c.ID != "" {
					entry["id"] = c.ID
				}
				if flow != "" {
					entry["flow"] = flow
				}
				if c.Reverse != nil {
					entry["reverse"] = c.Reverse
				}
			case model.VMESS:
				if c.ID != "" {
					entry["id"] = c.ID
				}
				if c.Security != "" {
					entry["security"] = c.Security
				}
			case model.Trojan:
				if c.Password != "" {
					entry["password"] = c.Password
				}
				if flow != "" {
					entry["flow"] = flow
				}
			case model.Shadowsocks:
				if c.Password != "" {
					entry["password"] = c.Password
				}
			case model.Hysteria:
				if c.Auth != "" {
					entry["auth"] = c.Auth
				}
			case model.MASQUE:
				if c.Password != "" {
					entry["password"] = c.Password
				}
			}
			finalClients = append(finalClients, entry)
		}

		_, hadClients := settings["clients"]
		mutated := hadClients || len(finalClients) > 0
		if mutated {
			settings["clients"] = finalClients
		}

		if inboundCanHostFallbacks(inbound) {
			fallbacks, fbErr := s.inboundService.fallbackService.BuildFallbacksJSON(nil, inbound.Id)
			if fbErr != nil {
				return nil, fbErr
			}
			if len(fallbacks) > 0 {
				generic := make([]any, 0, len(fallbacks))
				for _, f := range fallbacks {
					generic = append(generic, f)
				}
				settings["fallbacks"] = generic
				mutated = true
			}
		}

		if mutated {
			modifiedSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				return nil, err
			}
			inbound.Settings = string(modifiedSettings)
		}

		if len(inbound.StreamSettings) > 0 {
			// Unmarshal stream JSON
			var stream map[string]any
			json.Unmarshal([]byte(inbound.StreamSettings), &stream)

			// Remove the "settings" field under "tlsSettings" and "realitySettings"
			tlsSettings, ok1 := stream["tlsSettings"].(map[string]any)
			realitySettings, ok2 := stream["realitySettings"].(map[string]any)
			if ok1 || ok2 {
				if ok1 {
					delete(tlsSettings, "settings")
				} else if ok2 {
					delete(realitySettings, "settings")
				}
			}

			delete(stream, "externalProxy")

			newStream, err := json.MarshalIndent(stream, "", "  ")
			if err != nil {
				return nil, err
			}
			inbound.StreamSettings = string(newStream)
		}

		if inbound.Protocol == model.Shadowsocks {
			if healed, ok := model.HealShadowsocksClientMethods(inbound.Settings); ok {
				inbound.Settings = healed
			}
		}

		inboundConfig := inbound.GenXrayInboundConfig()
		xrayConfig.InboundConfigs = append(xrayConfig.InboundConfigs, *inboundConfig)
	}

	// Merge subscription-derived outbounds (if any) into the final outbounds array.
	// These are additive: each subscription is placed before or after the template
	// outbounds based on its Prepend flag, ordered by Priority. Tags assigned by the
	// subscription service are kept stable across refreshes so that balancers and
	// routing rules continue to work.
	subSvc := &OutboundSubscriptionService{}
	if prepend, appendList, err := subSvc.activeOutboundsSplit(); err == nil && (len(prepend) > 0 || len(appendList) > 0) {
		mergeSubscriptionOutbounds(xrayConfig, prepend, appendList)
	}

	// Route opted-in local mtproto inbounds through the core's router. Each one
	// gets a loopback SOCKS bridge — tagged with the inbound's own tag so it is
	// matchable in routing rules — that its mtg sidecar dials Telegram through.
	// Done after the subscription merge so a selected subscription outbound (or
	// balancer) is a valid rule target.
	for i := range inbounds {
		inbound := inbounds[i]
		if inbound.Protocol != model.MTProto || !inbound.Enable || inbound.NodeID != nil {
			continue
		}
		injectMtprotoEgress(xrayConfig, inbound)
	}

	// Every AmneziaWG inbound is embedded (internal/amneziawgnet: amneziawg-go
	// over a gVisor netstack, no kernel module) and relays every peer's
	// decapsulated traffic into its own loopback SOCKS5 inbound, always on —
	// unlike mtproto's bridge above, there's no opt-in gate here: once
	// traffic is decapsulated in gVisor, Xray's own freedom outbound is the
	// only way it reaches the real internet at all, not an optional extra
	// hop. Whether it goes anywhere beyond Xray's default routing is up to
	// whatever rules the admin adds through the stock Routing page, exactly
	// like routing any other protocol.
	injectAmneziawgnetSocks(xrayConfig, inbounds)
	injectTuicSocks(xrayConfig, inbounds)

	// Restores each opted-in peer's own distinct public IPv6 source identity
	// for its outbound connections — a peer that has an IPv6 address in its
	// AllowedIPs, on an inbound with IPv6Enabled, gets its own freedom
	// outbound bound to that exact address via sendThrough.
	// internal/amneziawgnet's own Manager is responsible for actually
	// aliasing that address onto the host (see v6alias.go) so the kernel
	// lets Xray bind an egress socket to it at all; this call only builds
	// the Xray-side outbound/routing-rule half.
	injectAmneziawgV6Egress(xrayConfig, inbounds)

	// Give every enabled local OpenVPN/L2TP inbound a loopback TPROXY relay
	// (tagged with the inbound's tag) so the daemons' network managers can
	// divert their clients' traffic into the router and the Routing page
	// applies to them too. See daemon_relay.go. Placed after the AmneziaWG
	// relays so its port picker sees their ports, and before the panel
	// egress bridge so that one sees ours.
	injectDaemonRelays(xrayConfig, inbounds)

	// Wire the panel's own HTTP traffic through the configured outbound, after
	// the subscription merge so subscription outbound tags are valid targets.
	if egressTag, err := s.settingService.GetPanelOutbound(); err != nil {
		logger.Warning("read panelOutbound setting failed:", err)
	} else if egressTag != "" {
		injectPanelEgress(xrayConfig, egressTag)
	}

	return xrayConfig, nil
}

// PanelEgressInboundTag is the tag of the loopback SOCKS inbound injected into
// the generated config when a panel outbound is configured. The panel's own
// HTTP clients dial through it to egress via the chosen outbound.
const PanelEgressInboundTag = "panel-egress"

// panelEgressBasePort is the first port tried for the egress bridge; ports
// already taken by other inbounds in the generated config are skipped.
const panelEgressBasePort = 62790

// injectPanelEgress appends a loopback SOCKS inbound to the generated config
// and prepends a routing rule sending it to outboundTag. Both live only in the
// generated config — the stored template is never modified — and both are
// hot-appliable, so changing the panel outbound never restarts the core.
func injectPanelEgress(cfg *xray.Config, outboundTag string) {
	for i := range cfg.InboundConfigs {
		if cfg.InboundConfigs[i].Tag == PanelEgressInboundTag {
			logger.Warning("panel egress: inbound tag [", PanelEgressInboundTag, "] already exists, skipping injection")
			return
		}
	}

	// The rule must exist before the inbound takes traffic, otherwise the
	// bridge would silently egress through the default outbound instead.
	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			logger.Warning("panel egress: routing section is unparsable, skipping injection:", err)
			return
		}
	}
	rules, _ := routing["rules"].([]any)
	rule := map[string]any{
		"type":       "field",
		"inboundTag": []any{PanelEgressInboundTag},
	}
	// The configured tag may name a routing balancer instead of a concrete
	// outbound. A field rule can target either, so emit the matching key —
	// balancerTag load-balances the panel's own traffic across the balancer's
	// outbounds, while a plain outbound tag keeps the original behavior.
	if routingTagIsBalancer(routing, outboundTag) {
		rule["balancerTag"] = outboundTag
	} else {
		rule["outboundTag"] = outboundTag
	}
	routing["rules"] = append([]any{rule}, rules...)
	newRouting, err := json.Marshal(routing)
	if err != nil {
		logger.Warning("panel egress: failed to rebuild routing section, skipping injection:", err)
		return
	}
	cfg.RouterConfig = json_util.RawMessage(newRouting)

	used := make(map[int]struct{}, len(cfg.InboundConfigs))
	for i := range cfg.InboundConfigs {
		used[cfg.InboundConfigs[i].Port] = struct{}{}
	}
	port := panelEgressBasePort
	for {
		if _, taken := used[port]; !taken {
			break
		}
		port++
	}

	cfg.InboundConfigs = append(cfg.InboundConfigs, xray.InboundConfig{
		Listen:   json_util.RawMessage(`"127.0.0.1"`),
		Port:     port,
		Protocol: "socks",
		Settings: json_util.RawMessage(`{"auth":"noauth","udp":false}`),
		Tag:      PanelEgressInboundTag,
	})
}

// routingTagIsBalancer reports whether tag names a balancer in the parsed
// routing section. The panel-egress rule targets a balancer via balancerTag and
// a concrete outbound via outboundTag, so the caller picks the key from this.
func routingTagIsBalancer(routing map[string]any, tag string) bool {
	if tag == "" {
		return false
	}
	balancers, ok := routing["balancers"].([]any)
	if !ok {
		return false
	}
	for _, b := range balancers {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, ok := bm["tag"].(string); ok && t == tag {
			return true
		}
	}
	return false
}

// mtprotoEgressSocksSettings is the loopback SOCKS server a routed mtproto
// inbound exposes for its mtg sidecar to dial Telegram through. mtg makes plain
// TCP connections, so UDP is left off (matching the panel egress bridge).
const mtprotoEgressSocksSettings = `{"auth":"noauth","udp":false}`

// injectMtprotoEgress wires one routed mtproto inbound into the generated
// config: it appends a loopback SOCKS inbound (tagged with the inbound's own tag,
// on the egress port persisted in settings) and, when an outbound is selected,
// prepends a routing rule sending that tag to it. Both live only in the generated
// config — the stored template is untouched — and both are hot-appliable, so
// toggling routing never forces a full Xray restart. Mirrors injectPanelEgress.
func injectMtprotoEgress(cfg *xray.Config, inbound *model.Inbound) {
	var parsed struct {
		RouteThroughXray bool   `json:"routeThroughXray"`
		RouteXrayPort    int    `json:"routeXrayPort"`
		OutboundTag      string `json:"outboundTag"`
	}
	if err := json.Unmarshal([]byte(inbound.Settings), &parsed); err != nil {
		return
	}
	if !parsed.RouteThroughXray || parsed.RouteXrayPort <= 0 || inbound.Tag == "" {
		return
	}
	tag := inbound.Tag
	for i := range cfg.InboundConfigs {
		if cfg.InboundConfigs[i].Tag == tag {
			logger.Warning("mtproto egress: inbound tag [", tag, "] already present in generated config, skipping bridge")
			return
		}
	}

	if parsed.OutboundTag != "" {
		routing := map[string]any{}
		parseOK := true
		if len(cfg.RouterConfig) > 0 {
			if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
				logger.Warning("mtproto egress: routing section is unparsable, skipping rule:", err)
				parseOK = false
			}
		}
		if parseOK {
			rules, _ := routing["rules"].([]any)
			rule := map[string]any{
				"type":       "field",
				"inboundTag": []any{tag},
			}
			if routingTagIsBalancer(routing, parsed.OutboundTag) {
				rule["balancerTag"] = parsed.OutboundTag
			} else {
				rule["outboundTag"] = parsed.OutboundTag
			}
			routing["rules"] = append([]any{rule}, rules...)
			if newRouting, err := json.Marshal(routing); err == nil {
				cfg.RouterConfig = json_util.RawMessage(newRouting)
			} else {
				logger.Warning("mtproto egress: failed to rebuild routing section, skipping rule:", err)
			}
		}
	}

	cfg.InboundConfigs = append(cfg.InboundConfigs, xray.InboundConfig{
		Listen:   json_util.RawMessage(`"127.0.0.1"`),
		Port:     parsed.RouteXrayPort,
		Protocol: "socks",
		Settings: json_util.RawMessage(mtprotoEgressSocksSettings),
		Tag:      tag,
	})
}

// amneziawgEgressSniffingSettings matches this fork's normal per-inbound
// default (see default.json's "mixed" inbound). Without this, domain-based
// Routing rules can never match this relay: the peer resolved DNS
// itself, through the tunnel, before ever sending a packet — by the time the
// embedded forwarder recovers the decapsulated traffic, the destination is
// already a bare IP, with no domain name attached at the network layer at
// all. Sniffing recovers it from the payload itself (TLS SNI / HTTP Host /
// QUIC) the same way it already does for every other inbound; without it,
// only tag/IP/network-based rules can ever match this traffic, and any
// domain rule above it in the list is silently unreachable.
const amneziawgEgressSniffingSettings = `{"enabled":true,"destOverride":["http","tls","quic","fakedns"]}`

// injectAmneziawgnetSocks gives every enabled AmneziaWG inbound with at
// least one qualifying peer its own loopback SOCKS5 inbound for the
// embedded (amneziawg-go) relay path (internal/amneziawgnet) -- always on,
// since there is no alternative datapath once traffic is decapsulated in
// gVisor: Xray's own freedom outbound is how it reaches the real internet at
// all (see internal/amneziawgnet/relay.go's doc comment, Finding 3 of the
// migration plan). Tagged with the inbound's own real tag: it's already
// selectable in the panel's stock Routing page (InboundService.GetInboundTags
// is protocol-blind), and per-inbound traffic totals
// (internal/web/service/inbound_traffic.go's addClientTraffic) match by
// exact tag -- reusing it isn't a style choice.
func injectAmneziawgnetSocks(cfg *xray.Config, inbounds []*model.Inbound) {
	existingTags := make(map[string]struct{}, len(cfg.InboundConfigs))
	for i := range cfg.InboundConfigs {
		existingTags[cfg.InboundConfigs[i].Tag] = struct{}{}
	}

	for _, inbound := range inbounds {
		if inbound.Protocol != model.AmneziaWG || !inbound.Enable || inbound.NodeID != nil {
			continue
		}
		inst, ok := amneziawg.InstanceFromInbound(inbound)
		if !ok {
			continue
		}
		if _, taken := existingTags[inbound.Tag]; taken {
			logger.Warning("amneziawgnet socks: inbound tag [", inbound.Tag, "] already present in generated config, skipping its relay inbound")
			continue
		}

		emails := make([]string, 0, len(inst.Peers))
		for _, p := range inst.Peers {
			if p.Email != "" {
				emails = append(emails, p.Email)
			}
		}
		if len(emails) == 0 {
			continue
		}

		settings, err := amneziawgnet.SocksInboundSettings(emails, amneziawgnet.SocksPassword())
		if err != nil {
			logger.Warning("amneziawgnet socks: building settings for inbound [", inbound.Tag, "]: ", err)
			continue
		}

		existingTags[inbound.Tag] = struct{}{}
		cfg.InboundConfigs = append(cfg.InboundConfigs, xray.InboundConfig{
			Listen:   json_util.RawMessage(`"127.0.0.1"`),
			Port:     amneziawgnet.SOCKSPortForInbound(inbound.Id),
			Protocol: "socks",
			Settings: json_util.RawMessage(settings),
			Sniffing: json_util.RawMessage(amneziawgEgressSniffingSettings),
			Tag:      inbound.Tag,
		})
	}
}

const (
	tuicEgressSocksSettings = `{"auth":"noauth","udp":true}`
	// TUIC clients can connect to an IP while TLS carries a different or unresolvable SNI.
	// Keep sniffed domains available for routing without replacing the requested destination.
	tuicEgressSniffingSettings = `{"enabled":true,"destOverride":["http","tls","quic","fakedns"],"routeOnly":true}`
)

func injectTuicSocks(cfg *xray.Config, inbounds []*model.Inbound) {
	existingTags := make(map[string]struct{}, len(cfg.InboundConfigs))
	for i := range cfg.InboundConfigs {
		existingTags[cfg.InboundConfigs[i].Tag] = struct{}{}
	}

	for _, inbound := range inbounds {
		if inbound.Protocol != model.TUIC || !inbound.Enable || inbound.NodeID != nil {
			continue
		}
		if _, taken := existingTags[inbound.Tag]; taken {
			logger.Warning("tuic socks: inbound tag [", inbound.Tag, "] already present in generated config, skipping its relay inbound")
			continue
		}

		existingTags[inbound.Tag] = struct{}{}
		cfg.InboundConfigs = append(cfg.InboundConfigs, xray.InboundConfig{
			Listen:   json_util.RawMessage(`"127.0.0.1"`),
			Port:     tuic.SOCKSPortForInbound(inbound.Id),
			Protocol: "socks",
			Settings: json_util.RawMessage(tuicEgressSocksSettings),
			Sniffing: json_util.RawMessage(tuicEgressSniffingSettings),
			Tag:      inbound.Tag,
		})
	}
}

// amneziawgV6EgressTag returns the stable, globally-unique freedom outbound
// tag for one peer's IPv6 source-identity egress. Stable across config
// regenerations (a pure function of two stable identifiers), so
// internal/xray/hot_diff.go's tag-keyed outbound/routing diffing recognizes
// "unchanged" rather than remove+recreate on every poll. The inbound.Id
// prefix is defense in depth, not load-bearing on its own: email is already
// enforced globally unique across the whole panel's client table
// (model.ClientRecord.Email has a gorm uniqueIndex) — kept anyway since it
// costs nothing and makes the tag self-describing, matching
// NodeEgressInboundTag's own style.
func amneziawgV6EgressTag(inboundID int, email string) string {
	return fmt.Sprintf("amneziawg-v6-%d-%s", inboundID, email)
}

// injectAmneziawgV6Egress gives every enabled, non-node-hosted AmneziaWG
// peer with an IPv6 AllowedIPs entry its own single-purpose freedom
// outbound, bound via sendThrough to that exact address, plus a routing
// rule sending only that peer's own traffic through it — restoring the
// per-client public IPv6 identity the hard cutover temporarily dropped
// (Phase 3.5 of the migration plan). Scoped to outbound source identity
// only: it depends on internal/amneziawgnet's own alias mechanism actually
// giving the host that address at the OS level (see v6alias.go's
// V6AliasesActive, the exact same gate this function uses below) — without
// that, sendThrough fails to bind and every connection through it errors
// outright (freedom.go's dial failure); there is no fallback outbound.
//
// The routing rule matches both inboundTag and user: SocksInboundSettings
// (used by injectAmneziawgnetSocks above) already authenticates each
// connection as the peer's own email via stock SOCKS5 auth, and a stock
// Xray SOCKS5 inbound sets that connection's stats/routing identity from
// the authenticated username — so "user" reliably isolates exactly one
// peer's traffic, the same building block Finding 3 of the migration plan
// already established for per-client stats.
//
// Modeled on injectNodeEgresses (the established N-per-slice inbound+rule
// precedent, not injectAmneziawgnetSocks itself, which only ever emits a
// single inbound and never touches outbounds/routing) and
// mergeSubscriptionOutbounds's unmarshal-append-remarshal pattern for
// cfg.OutboundConfigs. Synthetic rules are prepended ahead of whatever's
// already in the routing rules array, the same pattern injectNodeEgresses/
// injectMtprotoEgress already use for their own always-must-win infra
// rules — this never touches the admin's own saved Routing-page rule
// order.
func injectAmneziawgV6Egress(cfg *xray.Config, inbounds []*model.Inbound) {
	// Protocol is checked alongside Tag, not just Tag alone: a tag collision
	// with some unrelated (non-socks) inbound must not be mistaken for this
	// instance's own relay having been created.
	liveInboundTags := make(map[string]struct{}, len(cfg.InboundConfigs))
	for i := range cfg.InboundConfigs {
		if cfg.InboundConfigs[i].Protocol == "socks" {
			liveInboundTags[cfg.InboundConfigs[i].Tag] = struct{}{}
		}
	}

	var existingOutbounds []any
	if len(cfg.OutboundConfigs) > 0 {
		if err := json.Unmarshal(cfg.OutboundConfigs, &existingOutbounds); err != nil {
			logger.Warning("amneziawg v6 egress: outbounds section is unparsable, skipping injection:", err)
			return
		}
	}
	usedOutboundTags := make(map[string]struct{}, len(existingOutbounds))
	for _, o := range existingOutbounds {
		if m, ok := o.(map[string]any); ok {
			if t, ok := m["tag"].(string); ok {
				usedOutboundTags[t] = struct{}{}
			}
		}
	}

	routing := map[string]any{}
	if len(cfg.RouterConfig) > 0 {
		if err := json.Unmarshal(cfg.RouterConfig, &routing); err != nil {
			logger.Warning("amneziawg v6 egress: routing section is unparsable, skipping injection:", err)
			return
		}
	}
	rules, _ := routing["rules"].([]any)
	newRules := make([]any, 0)
	newOutbounds := make([]any, 0)

	for _, inbound := range inbounds {
		if inbound.Protocol != model.AmneziaWG || !inbound.Enable || inbound.NodeID != nil {
			continue
		}
		if _, live := liveInboundTags[inbound.Tag]; !live {
			// The relay inbound itself wasn't created this pass (e.g. a tag
			// collision inside injectAmneziawgnetSocks) -- no SOCKS5 inbound
			// exists for hot_diff.go's inboundTag match to ever fire against.
			continue
		}
		inst, ok := amneziawg.InstanceFromInbound(inbound)
		if !ok || !amneziawgnet.V6AliasesActive(inst) {
			continue
		}
		for _, p := range inst.Peers {
			if p.Email == "" {
				continue
			}
			v6 := amneziawg.FirstIPv6(p.AllowedIPs)
			if v6 == "" {
				continue
			}
			tag := amneziawgV6EgressTag(inbound.Id, p.Email)
			if _, taken := usedOutboundTags[tag]; taken {
				logger.Warning("amneziawg v6 egress: outbound tag [", tag, "] already exists, skipping peer [", p.Email, "]")
				continue
			}
			usedOutboundTags[tag] = struct{}{}
			newOutbounds = append(newOutbounds, map[string]any{
				"tag":         tag,
				"protocol":    "freedom",
				"sendThrough": v6,
				"settings":    map[string]any{},
			})
			newRules = append(newRules, map[string]any{
				"type":        "field",
				"inboundTag":  []any{inbound.Tag},
				"user":        []any{p.Email},
				"outboundTag": tag,
			})
		}
	}

	if len(newOutbounds) == 0 {
		return
	}

	merged := make([]any, 0, len(existingOutbounds))
	merged = append(merged, existingOutbounds...)
	merged = append(merged, newOutbounds...)
	combined, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		logger.Warning("amneziawg v6 egress: failed to rebuild outbounds section, skipping injection:", err)
		return
	}
	cfg.OutboundConfigs = json_util.RawMessage(combined)

	routing["rules"] = append(newRules, rules...)
	newRouting, err := json.Marshal(routing)
	if err != nil {
		logger.Warning("amneziawg v6 egress: failed to rebuild routing section, skipping injection:", err)
		return
	}
	cfg.RouterConfig = json_util.RawMessage(newRouting)
}

// mergeSubscriptionOutbounds appends the subscription outbounds to the
// OutboundConfigs array of the xray config. It works on the already-unmarshaled
// template so that manually configured outbounds are never overwritten.
//
// Safety: if we cannot parse the template's outbounds array, we leave
// OutboundConfigs exactly as it came from the template (we do not inject
// subscription outbounds). This prevents us from accidentally dropping the
// user's manually configured outbounds when the template is in a weird state.
func mergeSubscriptionOutbounds(cfg *xray.Config, prepend, appendList []any) {
	if len(prepend) == 0 && len(appendList) == 0 {
		return
	}
	var templateOutbounds []any
	if len(cfg.OutboundConfigs) > 0 {
		if err := json.Unmarshal(cfg.OutboundConfigs, &templateOutbounds); err != nil {
			// Corrupt template outbounds — do not touch the field at all.
			// The user will see problems on Xray start / next save.
			return
		}
	}
	merged := make([]any, 0, len(prepend)+len(templateOutbounds)+len(appendList))
	merged = append(merged, prepend...)
	merged = append(merged, templateOutbounds...)
	merged = append(merged, appendList...)
	combined, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return
	}
	cfg.OutboundConfigs = json_util.RawMessage(combined)
}

// ensureAPIServices guarantees the gRPC services the panel depends on are
// listed in the generated config's api block: HandlerService and StatsService
// have always been required for inbound/user management and traffic polling,
// and RoutingService enables hot routing reload on templates saved before it
// was added to the default template. The stored template itself is not
// modified — only the generated runtime config.
func ensureAPIServices(api json_util.RawMessage) json_util.RawMessage {
	if len(api) == 0 {
		// No api block means the panel's API integration is deliberately
		// disabled; don't resurrect it behind the user's back.
		return api
	}
	var parsed map[string]any
	if err := json.Unmarshal(api, &parsed); err != nil {
		return api
	}
	services, _ := parsed["services"].([]any)
	have := make(map[string]bool, len(services))
	for _, svc := range services {
		if name, ok := svc.(string); ok {
			have[name] = true
		}
	}
	added := false
	for _, name := range []string{"HandlerService", "StatsService", "RoutingService"} {
		if !have[name] {
			services = append(services, name)
			added = true
		}
	}
	if !added {
		return api
	}
	parsed["services"] = services
	out, err := json.Marshal(parsed)
	if err != nil {
		return api
	}
	return out
}

// ensureStatsPolicy guarantees every policy level in the generated config has
// statsUserOnline enabled, so the core tracks per-email online IPs for the
// panel's online view and access-log-free IP limiting. Generated clients carry
// no explicit level, so level "0" is created when absent. The flag is panel
// infrastructure and is forced on even over an explicit false in the template,
// same as the api services above. An entirely missing or unparsable policy
// block is left alone; the stored template itself is never modified — only the
// generated runtime config.
func ensureStatsPolicy(policy json_util.RawMessage) json_util.RawMessage {
	if len(policy) == 0 {
		return policy
	}
	var parsed map[string]any
	if err := json.Unmarshal(policy, &parsed); err != nil {
		return policy
	}
	levels, _ := parsed["levels"].(map[string]any)
	if levels == nil {
		levels = make(map[string]any)
	}
	if _, ok := levels["0"]; !ok {
		levels["0"] = map[string]any{}
	}
	changed := false
	for _, raw := range levels {
		level, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if enabled, ok := level["statsUserOnline"].(bool); !ok || !enabled {
			level["statsUserOnline"] = true
			changed = true
		}
	}
	if !changed {
		return policy
	}
	parsed["levels"] = levels
	out, err := json.Marshal(parsed)
	if err != nil {
		return policy
	}
	return out
}

func resolveXrayLogPaths(logCfg json_util.RawMessage) json_util.RawMessage {
	if len(logCfg) == 0 {
		return logCfg
	}
	var parsed map[string]any
	if err := json.Unmarshal(logCfg, &parsed); err != nil {
		return logCfg
	}
	changed := false
	for _, key := range []string{"access", "error"} {
		v, ok := parsed[key].(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(v)
		if trimmed == "" || strings.EqualFold(trimmed, "none") {
			continue
		}
		base := path.Base(filepath.ToSlash(trimmed))
		if base == "" || base == "." || base == ".." || base == "/" {
			continue
		}
		confined := filepath.Join(config.GetLogFolder(), base)
		if confined == trimmed {
			continue
		}
		parsed[key] = confined
		changed = true
	}
	if !changed {
		return logCfg
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return logCfg
	}
	return out
}

// GetXrayTraffic fetches the current traffic statistics from the running Xray process.
func (s *XrayService) GetXrayTraffic() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
	// The process lock serializes polls across all service instances, as well
	// as process replacement. The dedicated API shares its baseline globally.
	lock.Lock()
	defer lock.Unlock()
	// Do not poll again or discard the evidence on a core replacement while
	// an earlier incident could not be written to the database.
	if trafficState.pendingCounterDecrease != nil {
		return nil, nil, trafficState.persistPendingCounterDecrease(database.GetDB())
	}
	if trafficState.pendingCounterMissing != nil {
		return nil, nil, trafficState.persistPendingCounterMissing(database.GetDB())
	}
	current := p
	if current == nil || !current.IsRunning() {
		err := errors.New("xray is not running")
		logger.Debug("Attempted to fetch Xray traffic, but Xray is not running:", err)
		return nil, nil, err
	}
	trafficState.observeProcess(current)
	db := database.GetDB()
	// A marker from an older managed core used to block every later poll after
	// a panel/server restart. On a local SQLite panel we can safely take a new
	// no-charge baseline from the replacement core, archive the older evidence,
	// and resume forward-only accounting. Shared PostgreSQL deployments retain
	// the explicit reconciliation barrier because this process cannot identify
	// evidence written by another panel.
	present, recoveryEligible, recoveryErr := localXrayCounterIncidentRecoveryEligible(db, current.StartedAt())
	if recoveryErr != nil {
		return nil, nil, recoveryErr
	}
	if present && !recoveryEligible {
		if err := checkLocalXrayCounterIncidents(db); err != nil {
			return nil, nil, err
		}
	}
	if err := trafficState.api.Init(current.GetAPIPort()); err != nil {
		logger.Debug("Failed to initialize Xray API:", err)
		return nil, nil, err
	}
	defer trafficState.api.Close()
	if recoveryEligible {
		// Do not charge the current cumulative counter values to the period
		// before the replacement core was observed. The archived marker keeps
		// that unknown interval auditable; the next poll starts fresh.
		trafficState.api.ResetTrafficBaseline(false)
		if _, _, err := trafficState.api.GetTraffic(); err != nil {
			return nil, nil, fmt.Errorf("establish Xray recovery baseline: %w", err)
		}
		_, recovered, backupDir, recoveryErr := recoverLocalXrayCounterIncidentsAfterRestart(db, current.StartedAt())
		if recoveryErr != nil {
			return nil, nil, recoveryErr
		}
		if !recovered {
			return nil, nil, errors.New("local Xray accounting evidence changed during restart recovery")
		}
		logger.Warning("archived local Xray accounting evidence after a fresh restart baseline:", backupDir)
		return nil, nil, nil
	}
	traffic, clientTraffic, err := trafficState.api.GetTraffic()
	if err != nil {
		var decrease *xray.UserTrafficCounterDecrease
		if errors.As(err, &decrease) {
			trafficState.pendingCounterDecrease = decrease
			xrayCounterIncidentPending.Store(true)
			return nil, nil, trafficState.persistPendingCounterDecrease(database.GetDB())
		}
		var missing *xray.UserTrafficCounterMissing
		if errors.As(err, &missing) {
			trafficState.pendingCounterMissing = missing
			xrayCounterIncidentPending.Store(true)
			return nil, nil, trafficState.persistPendingCounterMissing(database.GetDB())
		}
		logger.Debug("Failed to fetch Xray traffic:", err)
		return nil, nil, err
	}
	traffic = dropDaemonRelayTraffic(current.GetConfig(), traffic)
	return traffic, clientTraffic, nil
}

// GetOnlineUsers returns connection-based online users (email + source IPs)
// from the running core's online-stats API. ok=false means the API is not
// available — xray isn't running or the core predates the online-stats RPCs —
// and callers must use the legacy traffic-delta / access-log paths. The
// capability is probed lazily per process: an Unimplemented answer pins this
// core as unsupported until the next restart, while transient errors leave the
// capability undecided so a flaky poll can't lock in legacy mode.
func (s *XrayService) GetOnlineUsers() ([]xray.OnlineUser, bool, error) {
	if !s.IsXrayRunning() {
		return nil, false, nil
	}
	if p.OnlineAPISupport() == xray.OnlineAPIUnsupported {
		return nil, false, nil
	}
	if err := s.xrayAPI.Init(p.GetAPIPort()); err != nil {
		logger.Debug("Failed to initialize Xray API:", err)
		return nil, false, err
	}
	defer s.xrayAPI.Close()

	users, err := s.xrayAPI.GetOnlineUsers()
	if err != nil {
		if xray.IsUnimplementedErr(err) {
			p.SetOnlineAPISupport(xray.OnlineAPIUnsupported)
			logger.Info("xray core does not support the online-stats API; falling back to traffic-delta onlines and access-log IP limit")
			return nil, false, nil
		}
		logger.Debug("Failed to fetch Xray online users:", err)
		return nil, false, err
	}
	if p.OnlineAPISupport() == xray.OnlineAPIUnknown {
		p.SetOnlineAPISupport(xray.OnlineAPISupported)
		logger.Info("xray core supports the online-stats API; using connection-based onlines and access-log-free IP limit")
	}
	return users, true, nil
}

// BalancerStatus is the live view of one balancer for the panel UI. Running
// is false when the balancer isn't present in the running core (e.g. xray is
// stopped or the balancer hasn't been saved/applied yet).
type BalancerStatus struct {
	Tag      string   `json:"tag"`
	Running  bool     `json:"running"`
	Override string   `json:"override"`
	Selected []string `json:"selected"`
}

// GetBalancersStatus queries the running core for the live state of the
// given balancer tags. Per-tag failures are reported as Running=false rather
// than failing the whole call, so the UI can render saved-but-not-applied
// balancers alongside live ones.
func (s *XrayService) GetBalancersStatus(tags []string) ([]BalancerStatus, error) {
	statuses := make([]BalancerStatus, 0, len(tags))
	if !s.IsXrayRunning() {
		for _, tag := range tags {
			statuses = append(statuses, BalancerStatus{Tag: tag})
		}
		return statuses, nil
	}
	if err := s.xrayAPI.Init(p.GetAPIPort()); err != nil {
		return nil, err
	}
	defer s.xrayAPI.Close()

	for _, tag := range tags {
		info, err := s.xrayAPI.GetBalancerInfo(tag)
		if err != nil {
			logger.Debug("get balancer info [", tag, "] failed:", err)
			statuses = append(statuses, BalancerStatus{Tag: tag})
			continue
		}
		statuses = append(statuses, BalancerStatus{
			Tag:      tag,
			Running:  true,
			Override: info.Override,
			Selected: info.Selected,
		})
	}
	return statuses, nil
}

// OverrideBalancer forces a balancer in the running core to use the given
// outbound tag; an empty target clears the override.
func (s *XrayService) OverrideBalancer(tag, target string) error {
	if !s.IsXrayRunning() {
		return errors.New("xray is not running")
	}
	if err := s.xrayAPI.Init(p.GetAPIPort()); err != nil {
		return err
	}
	defer s.xrayAPI.Close()
	return s.xrayAPI.SetBalancerTarget(tag, target)
}

// TestRoute asks the running core which outbound its router picks for the
// described connection.
func (s *XrayService) TestRoute(req xray.RouteTestRequest) (*xray.RouteTestResult, error) {
	if !s.IsXrayRunning() {
		return nil, errors.New("xray is not running")
	}
	if err := s.xrayAPI.Init(p.GetAPIPort()); err != nil {
		return nil, err
	}
	defer s.xrayAPI.Close()
	return s.xrayAPI.TestRoute(req)
}

// RestartXray reconciles the running Xray process with the current desired
// config. When isForce is false it first tries to apply the changes through
// the Xray gRPC API without restarting the process (inbounds, outbounds and
// routing rules/balancers are hot-reloadable); only changes the core cannot
// take at runtime — or a force request — stop and restart the process.
func (s *XrayService) RestartXray(isForce bool) error {
	defer s.SyncDaemonInbounds()
	lock.Lock()
	defer lock.Unlock()
	logger.Debug("restart Xray, force:", isForce)
	// Evaluate the relay crash-loop guard before the config is regenerated so
	// a suspension decided here already shapes this very config.
	wasManuallyStopped := isManuallyStopped.Load()
	crashed := p != nil && !p.IsRunning() && !wasManuallyStopped
	if crashed {
		// The dead core can no longer provide its final counters. Keep a
		// durable uncertainty marker rather than assuming its last poll was
		// complete. No traffic quantity is inferred from the exit.
		if err := recordUnexpectedXrayExit(database.GetDB()); err != nil {
			return err
		}
	}
	noteDaemonRelayCrash(isForce, crashed)
	isManuallyStopped.Store(false)

	xrayConfig, err := s.GetXrayConfig()
	if err != nil {
		return err
	}

	if s.IsXrayRunning() {
		configUnchanged := p.GetConfig().Equals(xrayConfig)
		if !isForce && configUnchanged && !isNeedXrayRestart.Load() {
			logger.Debug("It does not need to restart Xray")
			return nil
		}
		if !isForce && !configUnchanged && s.tryHotApply(xrayConfig) {
			logger.Info("Xray config changes applied through the core API, no restart needed")
			return nil
		}
		if stopErr := p.Stop(); stopErr != nil {
			// A failed stop leaves the core's final accounting uncertain even
			// when it still reports running.
			if !wasManuallyStopped {
				if markErr := recordUnexpectedXrayExit(database.GetDB()); markErr != nil {
					return fmt.Errorf("stop Xray: %v; record exit: %w", stopErr, markErr)
				}
			}
			return fmt.Errorf("stop Xray before restart: %w", stopErr)
		}
	} else if p != nil && !crashed && !wasManuallyStopped {
		// Config generation may have outlived a core that was running at
		// the first check. Preserve the uncertainty before replacing it.
		if err := recordUnexpectedXrayExit(database.GetDB()); err != nil {
			return err
		}
	}

	p = xray.NewProcess(xrayConfig)
	started := p
	started.SetOnUnexpectedExit(func() { rememberXrayExitObservation(started) })
	result = ""
	s.xrayAPI.StatsLastValues = nil
	err = p.Start()
	if err != nil {
		return err
	}
	go watchManagedXrayExit(started, started.Done())

	return nil
}

// watchManagedXrayExit notices a reaped managed core without waiting for the
// periodic watchdog. Stop/replacement is serialized with this check and must
// not be misclassified. A marker write failure leaves the in-memory barrier;
// the watchdog and restart path retry persistence.
func watchManagedXrayExit(started *xray.Process, done <-chan struct{}) {
	<-done
	defer func() {
		pendingXrayExitObservations.Lock()
		delete(pendingXrayExitObservations.processes, started)
		pendingXrayExitObservations.Unlock()
	}()
	if started.WasStoppedIntentionally() {
		return
	}
	// The process wait callback normally registered this before Done closed;
	// this also covers callers that do not install a callback.
	rememberXrayExitObservation(started)
	lock.Lock()
	defer lock.Unlock()
	if p != started || started.WasStoppedIntentionally() {
		return
	}
	// A manual-stop flag set after this core already exited does not prove
	// that its exit was intentional; rely on the process's own stop flag.
	if err := recordUnexpectedXrayExit(database.GetDB()); err != nil {
		logger.Error("Record unexpected Xray exit failed:", err)
	}
}

// tryHotApply attempts to reconcile the running Xray instance with newCfg
// through the core gRPC API (HandlerService for inbounds/outbounds,
// RoutingService for rules/balancers). It returns true when the running
// instance now matches newCfg; on any failure it returns false and the
// caller falls back to a full process restart, which cleans up whatever was
// partially applied. Callers must hold the package-level lock.
func (s *XrayService) tryHotApply(newCfg *xray.Config) bool {
	oldCfg := p.GetConfig()
	diff, ok := xray.ComputeHotDiff(oldCfg, newCfg)
	if !ok {
		logger.Debug("hot apply: config change is not API-applicable, falling back to restart")
		return false
	}
	if diff.Empty() {
		p.SetConfig(newCfg)
		return true
	}

	apiPort := p.GetAPIPort()
	if apiPort <= 0 {
		return false
	}
	// A dedicated client: s.xrayAPI may be in use by traffic polling on other
	// service instances and is reset around restarts.
	hotAPI := xray.XrayAPI{}
	if err := hotAPI.Init(apiPort); err != nil {
		logger.Debug("hot apply: failed to init xray api:", err)
		return false
	}
	defer hotAPI.Close()

	// Removals first so changed handlers and port swaps never collide with
	// the additions that follow.
	for _, tag := range diff.RemovedInboundTags {
		if err := hotAPI.DelInbound(tag); err != nil && !xray.IsMissingHandlerErr(err) {
			logger.Info("hot apply: remove inbound [", tag, "] failed:", err)
			return false
		}
	}
	for _, tag := range diff.RemovedOutboundTags {
		if err := hotAPI.DelOutbound(tag); err != nil && !xray.IsMissingHandlerErr(err) {
			logger.Info("hot apply: remove outbound [", tag, "] failed:", err)
			return false
		}
	}
	for _, ob := range diff.AddedOutbounds {
		if err := addOutboundReconciling(&hotAPI, ob); err != nil {
			logger.Info("hot apply: add outbound failed:", err)
			return false
		}
	}
	for _, ib := range diff.AddedInbounds {
		if err := addInboundReconciling(&hotAPI, ib); err != nil {
			logger.Info("hot apply: add inbound failed:", err)
			return false
		}
	}
	if diff.RoutingConfig != nil {
		if err := hotAPI.ApplyRoutingConfig(diff.RoutingConfig); err != nil {
			logger.Info("hot apply: apply routing config failed:", err)
			return false
		}
	}

	p.SetConfig(newCfg)
	return true
}

// addInboundReconciling adds an inbound, and on a tag conflict (the handler
// was already created through the runtime API while the stored snapshot was
// stale) replaces the existing handler instead.
func addInboundReconciling(api *xray.XrayAPI, inbound []byte) error {
	err := api.AddInbound(inbound)
	if err == nil || !xray.IsExistingTagErr(err) {
		return err
	}
	var meta struct {
		Tag string `json:"tag"`
	}
	if jsonErr := json.Unmarshal(inbound, &meta); jsonErr != nil || meta.Tag == "" {
		return err
	}
	if delErr := api.DelInbound(meta.Tag); delErr != nil && !xray.IsMissingHandlerErr(delErr) {
		return delErr
	}
	return api.AddInbound(inbound)
}

// addOutboundReconciling mirrors addInboundReconciling for outbounds.
func addOutboundReconciling(api *xray.XrayAPI, outbound []byte) error {
	err := api.AddOutbound(outbound)
	if err == nil || !xray.IsExistingTagErr(err) {
		return err
	}
	var meta struct {
		Tag string `json:"tag"`
	}
	if jsonErr := json.Unmarshal(outbound, &meta); jsonErr != nil || meta.Tag == "" {
		return err
	}
	if delErr := api.DelOutbound(meta.Tag); delErr != nil && !xray.IsMissingHandlerErr(delErr) {
		return delErr
	}
	return api.AddOutbound(outbound)
}

// StopXray stops the running Xray process.
func (s *XrayService) StopXray() error {
	lock.Lock()
	defer lock.Unlock()
	wasManuallyStopped := isManuallyStopped.Load()
	logger.Debug("Attempting to stop Xray...")
	if s.IsXrayRunning() {
		isManuallyStopped.Store(true)
		err := p.Stop()
		if err != nil && !wasManuallyStopped {
			if markErr := recordUnexpectedXrayExit(database.GetDB()); markErr != nil {
				return fmt.Errorf("stop Xray: %v; record exit: %w", err, markErr)
			}
		}
		return err
	}
	// A stop request after an unexpected exit must not suppress the watcher
	// or let a subsequent restart erase the unresolved accounting interval.
	if p != nil && !wasManuallyStopped {
		if err := recordUnexpectedXrayExit(database.GetDB()); err != nil {
			return err
		}
	}
	isManuallyStopped.Store(true)
	return errors.New("xray is not running")
}

// SetToNeedRestart marks that Xray needs to be restarted.
func (s *XrayService) SetToNeedRestart() {
	isNeedXrayRestart.Store(true)
}

// GetXrayAPIPort returns the port the local xray process is listening on
// for its gRPC HandlerService, or 0 when xray isn't currently running.
// Exposed for the runtime package's LocalRuntime adapter — runtime can't
// reach into the package-level `p` directly without a service-package
// import cycle.
func (s *XrayService) GetXrayAPIPort() int {
	if p == nil || !p.IsRunning() {
		return 0
	}
	return p.GetAPIPort()
}

// checkXrayAPIWhileLocked probes the managed process with a dedicated API
// client. It must be called while lock is held so a process replacement cannot
// make a successful response look like proof for a different core.
func checkXrayAPIWhileLocked(ctx context.Context, current *xray.Process) error {
	if current == nil || !current.IsRunning() {
		return errors.New("xray is not running")
	}
	port := current.GetAPIPort()
	if port <= 0 {
		return errors.New("xray API port is unavailable")
	}
	api := xray.XrayAPI{}
	if err := api.Init(port); err != nil {
		return err
	}
	defer api.Close()
	return api.HealthCheck(ctx)
}

// CheckXrayAPI verifies that the live core answers a bounded, read-only API
// request. It does not poll counters and therefore never changes accounting
// baselines. The watchdog uses this to detect the "process exists but API port
// is refusing connections" failure mode.
func (s *XrayService) CheckXrayAPI(ctx context.Context) error {
	lock.Lock()
	defer lock.Unlock()
	return checkXrayAPIWhileLocked(ctx, p)
}

// RecordUnreachableXrayAPI confirms an API outage while holding the process
// lock, then persists an uncertainty marker before the watchdog restarts the
// core. A response that recovers between watchdog ticks is deliberately not
// marked. The caller may only restart after this returns recorded=true.
func (s *XrayService) RecordUnreachableXrayAPI() (recorded bool, err error) {
	lock.Lock()
	defer lock.Unlock()
	if p == nil || !p.IsRunning() || isManuallyStopped.Load() {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := checkXrayAPIWhileLocked(ctx, p); err == nil {
		return false, nil
	}
	if err := recordUnreachableXrayAPI(database.GetDB()); err != nil {
		return false, err
	}
	return true, nil
}

// IsNeedRestartAndSetFalse checks if restart is needed and resets the flag to false.
func (s *XrayService) IsNeedRestartAndSetFalse() bool {
	return isNeedXrayRestart.CompareAndSwap(true, false)
}

// ObserveUnexpectedXrayExit lets the watchdog persist the uncertainty marker
// on its first observation, rather than waiting for its restart threshold.
// A nil process means the panel has not yet started a managed core; preserve
// the watchdog's existing startup behavior without inventing an exit.
func (s *XrayService) ObserveUnexpectedXrayExit() (bool, error) {
	lock.Lock()
	defer lock.Unlock()
	if isManuallyStopped.Load() || (p != nil && p.IsRunning()) {
		return false, nil
	}
	if p == nil {
		return true, nil
	}
	if err := recordUnexpectedXrayExit(database.GetDB()); err != nil {
		return true, err
	}
	return true, nil
}

// DidXrayCrash checks if Xray crashed by verifying it's not running and wasn't manually stopped.
func (s *XrayService) DidXrayCrash() bool {
	return !s.IsXrayRunning() && !isManuallyStopped.Load()
}

// persistPendingCounterDecrease runs under the process lock. Keep the original
// observation until the database confirms it is durable; never replace it with
// a later, possibly higher reading. A process crash can still lose this memory.
func (t *xrayTrafficState) persistPendingCounterDecrease(db *gorm.DB) error {
	decrease := t.pendingCounterDecrease
	if decrease == nil {
		return nil
	}
	diskErr := storeCounterIncidentFallback(localXrayIncidentEvidence(decrease))
	if err := recordLocalXrayCounterIncident(db, decrease); err != nil {
		if diskErr != nil {
			return fmt.Errorf("%w; database incident write failed: %v; fallback failed: %v", decrease, err, diskErr)
		}
		return fmt.Errorf("%w; database incident write failed: %v; fallback retained for manual review", decrease, err)
	}
	t.pendingCounterDecrease = nil
	if t == &trafficState {
		xrayCounterIncidentPending.Store(false)
	}
	if diskErr != nil {
		return fmt.Errorf("%w; fallback failed: %v; database incident retained", decrease, diskErr)
	}
	return decrease
}

// A missing counter has no observed value. Direction distinguishes this from
// an actual zero; fields named Observed* are not evidence for missing values.
func (t *xrayTrafficState) persistPendingCounterMissing(db *gorm.DB) error {
	missing := t.pendingCounterMissing
	if missing == nil {
		return nil
	}
	if transferEmailKey(missing.Email) == "" || missing.Previous < 0 || (missing.Direction != "uplink" && missing.Direction != "downlink") {
		return fmt.Errorf("%w; invalid missing Xray counter evidence", missing)
	}
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Email: transferEmailKey(missing.Email), Direction: "missing-" + missing.Direction}
	if missing.Direction == "uplink" {
		incident.PreviousUp = missing.Previous
	} else {
		incident.PreviousDown = missing.Previous
	}
	diskErr := storeCounterIncidentFallback(incident)
	if db == nil {
		return fmt.Errorf("%w; database unavailable; fallback error: %v", missing, diskErr)
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error; err != nil {
		return fmt.Errorf("%w; database incident write failed: %v; fallback error: %v", missing, err, diskErr)
	}
	t.pendingCounterMissing = nil
	if t == &trafficState {
		xrayCounterIncidentPending.Store(false)
	}
	if diskErr != nil {
		return fmt.Errorf("%w; fallback failed: %v; database incident retained", missing, diskErr)
	}
	return missing
}

// These markers persist until evidence-based reconciliation. An unobserved
// direction is not zero usage: Direction identifies the only measured pair.
func recordLocalXrayCounterIncident(db *gorm.DB, decrease *xray.UserTrafficCounterDecrease) error {
	if db == nil || decrease == nil || transferEmailKey(decrease.Email) == "" || decrease.Previous < 0 || decrease.Observed < 0 || decrease.Observed >= decrease.Previous {
		return errors.New("invalid local Xray counter incident")
	}
	incident := localXrayIncidentEvidence(decrease)
	switch decrease.Direction {
	case "uplink":
		incident.PreviousUp, incident.ObservedUp = decrease.Previous, decrease.Observed
	case "downlink":
		incident.PreviousDown, incident.ObservedDown = decrease.Previous, decrease.Observed
	default:
		return errors.New("invalid local Xray counter direction")
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error
}

func localXrayIncidentEvidence(decrease *xray.UserTrafficCounterDecrease) model.ResellerNodeCounterIncident {
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Email: transferEmailKey(decrease.Email), Direction: decrease.Direction}
	if decrease.Direction == "uplink" {
		incident.PreviousUp, incident.ObservedUp = decrease.Previous, decrease.Observed
	}
	if decrease.Direction == "downlink" {
		incident.PreviousDown, incident.ObservedDown = decrease.Previous, decrease.Observed
	}
	return incident
}

func checkLocalXrayCounterIncidents(db *gorm.DB) error {
	present, err := localXrayCounterIncidentPresent(db)
	if err != nil {
		return err
	}
	if present {
		return errors.New("local Xray counter incident requires manual reconciliation")
	}
	return nil
}

// An unexpected managed-core exit may have lost traffic since the last poll.
// The marker is global because the affected users and missing bytes are not
// knowable from the process exit. NodeId 0 denotes local Xray, empty Email is
// deliberately not a client identity, and Direction identifies this incident.
func recordUnexpectedXrayExit(db *gorm.DB) error {
	xrayCoreExitPending.Store(true)
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Email: "", Direction: "core-exited"}
	diskErr := storeCounterIncidentFallback(incident)
	if db == nil {
		return errors.New("cannot persist unexpected Xray exit: database unavailable")
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error; err != nil {
		return fmt.Errorf("cannot persist unexpected Xray exit: %w; fallback error: %v", err, diskErr)
	}
	xrayCoreExitPending.Store(false)
	if diskErr != nil {
		logger.Warning("unexpected Xray exit: local fallback failed; database incident retained:", diskErr)
	}
	return nil
}

// recordUnreachableXrayAPI preserves the fact that the panel could not read
// the core counters before automated recovery stops the process. A listening
// process alone is not accounting evidence: without its API, bytes since the
// last successful poll are unknowable. The marker therefore fails quota
// changes closed until the existing backup-first reconciliation flow reviews
// it, rather than inventing a zero or silently continuing.
func recordUnreachableXrayAPI(db *gorm.DB) error {
	xrayAPIOutagePending.Store(true)
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Email: "", Direction: "api-unreachable"}
	diskErr := storeCounterIncidentFallback(incident)
	if db == nil {
		return errors.New("cannot persist unreachable Xray API: database unavailable")
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error; err != nil {
		return fmt.Errorf("cannot persist unreachable Xray API: %w; fallback error: %v", err, diskErr)
	}
	xrayAPIOutagePending.Store(false)
	if diskErr != nil {
		logger.Warning("unreachable Xray API: local fallback failed; database incident retained:", diskErr)
	}
	return nil
}
