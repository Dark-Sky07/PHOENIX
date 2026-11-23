// Package pppvpn contains the shared local PPP tunnel runtime used by SSTP
// (sstpd) and legacy PPTP (pptpd). Both protocols are host daemons, not Xray
// inbounds; their client traffic is routed through the daemon relay when one
// is configured.
package pppvpn

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

type Protocol string

const (
	SSTP Protocol = "sstp"
	PPTP Protocol = "pptp"
)

type Credential struct {
	Username string
	Password string
}

type Instance struct {
	ID               int
	Tag              string
	Protocol         Protocol
	Listen           string
	Port             int
	LocalIP          string
	PoolCIDR         string
	DNS1             string
	DNS2             string
	CertificatePEM   string
	PrivateKeyPEM    string
	OutboundTag      string
	RouteThroughXray bool
	XrayRelayPort    int
	Credentials      []Credential
}

func DefaultPort(protocol Protocol) int {
	if protocol == SSTP {
		return 443
	}
	if protocol == PPTP {
		return 1723
	}
	return 0
}

func InstanceFromInbound(ib *model.Inbound, clients []model.Client) (Instance, bool) {
	if ib == nil || (ib.Protocol != model.SSTP && ib.Protocol != model.PPTP) {
		return Instance{}, false
	}
	var raw struct {
		LocalIP          string         `json:"localIP"`
		PoolCIDR         string         `json:"poolCIDR"`
		DNS1             string         `json:"dns1"`
		DNS2             string         `json:"dns2"`
		CertificatePEM   string         `json:"certificatePEM"`
		PrivateKeyPEM    string         `json:"privateKeyPEM"`
		OutboundTag      string         `json:"outboundTag"`
		RouteThroughXray *bool          `json:"routeThroughXray"`
		Clients          []model.Client `json:"clients"`
	}
	if strings.TrimSpace(ib.Settings) != "" {
		if err := json.Unmarshal([]byte(ib.Settings), &raw); err != nil {
			return Instance{}, false
		}
	}
	if clients == nil {
		clients = raw.Clients
	}
	proto := Protocol(ib.Protocol)
	localIP, pool := raw.LocalIP, raw.PoolCIDR
	if localIP == "" {
		if proto == SSTP {
			localIP = "10.254.0.1"
		} else {
			localIP = "10.253.0.1"
		}
	}
	if pool == "" {
		if proto == SSTP {
			pool = "10.254.0.0/24"
		} else {
			pool = "10.253.0.0/24"
		}
	}
	dns1, dns2 := raw.DNS1, raw.DNS2
	if dns1 == "" {
		dns1 = "1.1.1.1"
	}
	if dns2 == "" {
		dns2 = "8.8.8.8"
	}
	route := raw.RouteThroughXray != nil && *raw.RouteThroughXray
	creds := make([]Credential, 0, len(clients))
	for _, c := range clients {
		if c.Enable && strings.TrimSpace(c.Email) != "" && c.Password != "" {
			creds = append(creds, Credential{Username: c.Email, Password: c.Password})
		}
	}
	port := ib.Port
	if port <= 0 {
		port = DefaultPort(proto)
	}
	inst := Instance{ID: ib.Id, Tag: ib.Tag, Protocol: proto, Listen: strings.TrimSpace(ib.Listen), Port: port,
		LocalIP: localIP, PoolCIDR: pool, DNS1: dns1, DNS2: dns2, CertificatePEM: raw.CertificatePEM,
		PrivateKeyPEM: raw.PrivateKeyPEM, OutboundTag: strings.TrimSpace(raw.OutboundTag), RouteThroughXray: route, Credentials: creds}
	return inst, Validate(inst) == nil
}

func Validate(i Instance) error {
	if i.ID <= 0 {
		return errors.New("pppvpn: inbound id must be positive")
	}
	if i.Protocol != SSTP && i.Protocol != PPTP {
		return fmt.Errorf("pppvpn: unsupported protocol %q", i.Protocol)
	}
	if i.Port < 1 || i.Port > 65535 {
		return errors.New("pppvpn: port must be between 1 and 65535")
	}
	local := net.ParseIP(strings.TrimSpace(i.LocalIP))
	if local == nil || local.To4() == nil {
		return errors.New("pppvpn: localIP must be an IPv4 address")
	}
	poolIP, pool, err := net.ParseCIDR(strings.TrimSpace(i.PoolCIDR))
	if err != nil || poolIP.To4() == nil || pool.IP.To4() == nil {
		return errors.New("pppvpn: poolCIDR must be an IPv4 /24")
	}
	ones, bits := pool.Mask.Size()
	if ones != 24 || bits != 32 || !poolIP.Equal(pool.IP) {
		return errors.New("pppvpn: poolCIDR must be a network-aligned IPv4 /24")
	}
	if !pool.Contains(local) {
		return errors.New("pppvpn: localIP must be inside poolCIDR")
	}
	serverIP := append(net.IP(nil), pool.IP.To4()...)
	serverIP[3]++
	if !local.Equal(serverIP) {
		return errors.New("pppvpn: localIP must be the first host address in poolCIDR")
	}
	for name, value := range map[string]string{"dns1": i.DNS1, "dns2": i.DNS2} {
		ip := net.ParseIP(strings.TrimSpace(value))
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("pppvpn: %s must be an IPv4 address", name)
		}
	}
	if i.Listen != "" && i.Listen != "0.0.0.0" {
		ip := net.ParseIP(i.Listen)
		if ip == nil || ip.To4() == nil {
			return errors.New("pppvpn: listen must be an IPv4 address")
		}
	}
	if i.Protocol == SSTP {
		if _, err := tls.X509KeyPair([]byte(i.CertificatePEM), []byte(i.PrivateKeyPEM)); err != nil {
			return fmt.Errorf("pppvpn: invalid SSTP certificate/key pair: %w", err)
		}
		cert, err := tls.X509KeyPair([]byte(i.CertificatePEM), []byte(i.PrivateKeyPEM))
		if err != nil || len(cert.Certificate) == 0 {
			return errors.New("pppvpn: invalid SSTP certificate")
		}
		if _, err := x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return errors.New("pppvpn: invalid SSTP certificate")
		}
	}
	for _, c := range i.Credentials {
		if strings.TrimSpace(c.Username) == "" || c.Password == "" || strings.ContainsAny(c.Username, "\r\n*\t") || strings.ContainsAny(c.Password, "\r\n") {
			return errors.New("pppvpn: invalid PPP credential")
		}
	}
	return nil
}

func (i Instance) fingerprint() string {
	encoded, _ := json.Marshal(struct {
		Protocol                                                             Protocol
		Port                                                                 int
		Listen, LocalIP, PoolCIDR, DNS1, DNS2, CertificatePEM, PrivateKeyPEM string
		Credentials                                                          []Credential
	}{i.Protocol, i.Port, i.Listen, i.LocalIP, i.PoolCIDR, i.DNS1, i.DNS2, i.CertificatePEM, i.PrivateKeyPEM, i.Credentials})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
