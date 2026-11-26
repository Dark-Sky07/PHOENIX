// Package openvpn manages openvpn daemon processes that serve OpenVPN
// inbounds. Xray-core has no OpenVPN protocol, so openvpn inbounds are run as
// standalone openvpn server processes — one process per inbound — entirely
// outside the Xray config and lifecycle. The pattern mirrors the mtproto
// (mtg) sidecar manager: a reconcile job keeps the running set in sync with
// the database and folds daemon-reported traffic into the normal accounting.
//
// Every inbound gets its own data directory holding a self-signed CA, the
// server keypair and one keypair per attached client (CN = client email).
// Certificates are generated in pure Go, so no openssl is required.
package openvpn

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

// Instance is the desired runtime configuration of one openvpn inbound.
type Instance struct {
	Id     int
	Tag    string
	Listen string
	Port   int

	// Proto is the transport the daemon listens on: "udp" or "tcp".
	Proto string

	// RedirectGw pushes "redirect-gateway def1 bypass-dns" so the client's
	// whole default route goes through the tunnel.
	RedirectGw bool

	// PushDNS pushes the configured DNS servers to the clients.
	PushDNS bool
	DNS1    string
	DNS2    string

	// Clients are the emails of the clients attached to this inbound; each
	// becomes one certificate (CN = email) and one management-interface
	// identity, which is also how per-client traffic is attributed.
	Clients []string

	// XrayRelayPort is the loopback TPROXY listener Xray currently exposes
	// for this inbound (0 = none). When set, the network manager diverts
	// the pool's TCP/UDP traffic into Xray so the panel's routing rules apply
	// to it. It is deliberately not part of fingerprint(): it only changes
	// iptables rules, never the daemon config, so it must never restart a
	// daemon with live sessions.
	XrayRelayPort int
}

// fingerprint changes whenever any value that ends up in the generated
// openvpn.conf — or the set of client certificates — changes, so reconcile
// restarts the daemon when the operator edits a setting or (de)attaches a
// client.
func (inst Instance) fingerprint() string {
	clients := make([]string, len(inst.Clients))
	copy(clients, inst.Clients)
	sort.Strings(clients)
	encoded, _ := json.Marshal(struct {
		Port                int
		Listen, Proto       string
		RedirectGw, PushDNS bool
		DNS1, DNS2          string
		Clients             []string
	}{inst.Port, inst.listenFor(), inst.protoFor(), inst.RedirectGw, inst.PushDNS, inst.dns1For(), inst.dns2For(), clients})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (inst Instance) listenFor() string {
	if inst.Listen == "" || inst.Listen == "0.0.0.0" {
		return ""
	}
	return inst.Listen
}

func (inst Instance) protoFor() string {
	if strings.HasPrefix(inst.Proto, "tcp") {
		return "tcp"
	}
	return "udp"
}

func (inst Instance) dns1For() string {
	if inst.DNS1 != "" {
		return inst.DNS1
	}
	return "1.1.1.1"
}

func (inst Instance) dns2For() string {
	if inst.DNS2 != "" {
		return inst.DNS2
	}
	return "8.8.8.8"
}

// safeConfigHost restricts unquoted OpenVPN host operands to IP literals or
// ASCII DNS-like names. This rejects whitespace, comments, quotes and control
// characters rather than trying to escape attacker-controlled directives.
func safeConfigHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	ipHost := host
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		ipHost = host[1 : len(host)-1]
	}
	if net.ParseIP(ipHost) != nil {
		return true
	}
	dnsHost := strings.TrimSuffix(host, ".") // permit fully qualified DNS names
	for _, label := range strings.Split(dnsHost, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
				return false
			}
		}
	}
	return true
}

func (inst Instance) validateRuntimeConfig() error {
	if !validInboundID(inst.Id) {
		return fmt.Errorf("openvpn: inbound ID %d cannot be assigned a unique legacy subnet", inst.Id)
	}
	if inst.Port < 1 || inst.Port > 65535 {
		return fmt.Errorf("openvpn: invalid port %d", inst.Port)
	}
	for _, ch := range dataDirForID(inst.Id) {
		if unicode.IsControl(ch) {
			return fmt.Errorf("openvpn: control character in data directory")
		}
	}
	if inst.listenFor() != "" && !safeConfigHost(inst.listenFor()) {
		return fmt.Errorf("openvpn: invalid listen address")
	}
	if inst.PushDNS && (net.ParseIP(inst.dns1For()) == nil || net.ParseIP(inst.dns2For()) == nil) {
		return fmt.Errorf("openvpn: DNS servers must be IP addresses")
	}
	return nil
}

// The legacy subnet scheme is injective only below 64000. Higher IDs alias
// an earlier subnet because the third octet was historically clamped to 255.
// Reject them instead of silently changing existing deployments' addresses.
func validInboundID(id int) bool { return id > 0 && id < 64000 }

// serverSubnet returns the legacy /24 for a validated inbound ID.
func (inst Instance) serverSubnet() (network string, mask string) {
	n := inst.Id % 250
	o := inst.Id / 250
	if o > 255 {
		o = 255
	}
	return "10." + strconv.Itoa(n) + "." + strconv.Itoa(o) + ".0", "255.255.255.0"
}

func (inst Instance) serverSubnetCIDR() string {
	network, _ := inst.serverSubnet()
	return network + "/24"
}

// InstanceFromInbound derives a desired Instance from an openvpn inbound.
// Returns false when the inbound is not a usable openvpn inbound.
func InstanceFromInbound(ib *model.Inbound, clients []string) (Instance, bool) {
	if ib == nil || ib.Protocol != model.OpenVPN || !validInboundID(ib.Id) {
		return Instance{}, false
	}
	var parsed struct {
		Proto      string `json:"proto"`
		RedirectGw *bool  `json:"redirectGateway"`
		PushDNS    *bool  `json:"pushDNS"`
		DNS1       string `json:"dns1"`
		DNS2       string `json:"dns2"`
	}
	if ib.Settings != "" {
		if err := json.Unmarshal([]byte(ib.Settings), &parsed); err != nil {
			return Instance{}, false
		}
	}
	// Both toggles default on (full tunnel + pushed DNS); the pointer form
	// distinguishes an explicit false from a missing key.
	redirectGw := true
	if parsed.RedirectGw != nil {
		redirectGw = *parsed.RedirectGw
	}
	pushDNS := true
	if parsed.PushDNS != nil {
		pushDNS = *parsed.PushDNS
	}
	inst := Instance{
		Id:         ib.Id,
		Tag:        ib.Tag,
		Listen:     ib.Listen,
		Port:       ib.Port,
		Proto:      parsed.Proto,
		RedirectGw: redirectGw,
		PushDNS:    pushDNS,
		DNS1:       parsed.DNS1,
		DNS2:       parsed.DNS2,
		Clients:    clients,
	}
	if inst.validateRuntimeConfig() != nil {
		return Instance{}, false
	}
	return inst, true
}
