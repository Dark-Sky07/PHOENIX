package pppvpn

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/util/tproxy"
)

type runner func(string, ...string) ([]byte, error)
type firewall struct {
	mu       sync.Mutex
	run      runner
	lookPath func(string) (string, error)
	states   map[int]Instance
}

var netOnce sync.Once
var netMgr *firewall

func networkManager() *firewall {
	netOnce.Do(func() {
		netMgr = &firewall{run: func(n string, a ...string) ([]byte, error) { return exec.Command(n, a...).CombinedOutput() }, lookPath: exec.LookPath, states: map[int]Instance{}}
	})
	return netMgr
}

func (m *firewall) iptables() (string, error) {
	for _, n := range []string{"iptables", "iptables-legacy", "iptables-nft"} {
		if p, e := m.lookPath(n); e == nil {
			return p, nil
		}
	}
	return "", errors.New("iptables binary not found")
}
func (m *firewall) runRule(bin, table, op string, rule []string) error {
	args := make([]string, 0, len(rule)+3)
	if table != "" {
		args = append(args, "-t", table)
	}
	args = append(args, op)
	args = append(args, rule...)
	out, e := m.run(bin, args...)
	if e != nil {
		return fmt.Errorf("iptables %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), e)
	}
	return nil
}
func (m *firewall) ensure(bin, table string, rule []string) error {
	if m.runRule(bin, table, "-C", rule) == nil {
		return nil
	}
	return m.runRule(bin, table, "-I", rule)
}
func (m *firewall) remove(bin, table string, rule []string) { _ = m.runRule(bin, table, "-D", rule) }
func inputRules(i Instance) [][]string {
	r := [][]string{{"INPUT", "-p", "tcp", "--dport", strconv.Itoa(i.Port), "-j", "ACCEPT"}}
	if i.Protocol == PPTP {
		r = append(r, []string{"INPUT", "-p", "gre", "-j", "ACCEPT"})
	}
	return r
}
func directRules(i Instance) [][]string {
	pool := i.PoolCIDR
	return [][]string{
		{"FORWARD", "-s", pool, "-j", "ACCEPT"},
		{"FORWARD", "-d", pool, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		{"POSTROUTING", "-s", pool, "-j", "MASQUERADE"},
	}
}
func relayRules(i Instance) []struct {
	table string
	rule  []string
} {
	var out []struct {
		table string
		rule  []string
	}
	for _, r := range tproxy.AcceptRules("ppp+", i.PoolCIDR) {
		out = append(out, struct {
			table string
			rule  []string
		}{"filter", r})
	}
	for _, r := range tproxy.DivertRules("ppp+", i.PoolCIDR, i.XrayRelayPort) {
		out = append(out, struct {
			table string
			rule  []string
		}{tproxy.DivertTable, r})
	}
	return out
}
func (m *firewall) apply(i Instance) error {
	bin, err := m.iptables()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = m.run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		if _, fallbackErr := m.run("sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"); fallbackErr != nil {
			return fmt.Errorf("enable IPv4 forwarding: %w", err)
		}
	}
	owner := string(i.Protocol) + ":" + strconv.Itoa(i.ID)
	for _, r := range inputRules(i) {
		if err = m.ensure(bin, "", r); err != nil {
			return err
		}
	}
	for _, r := range directRules(i) {
		table := ""
		if r[0] == "POSTROUTING" {
			table = "nat"
		}
		if err = m.ensure(bin, table, r); err != nil {
			return err
		}
	}
	if i.XrayRelayPort > 0 {
		if tproxy.FirewalldActive(tproxy.Runner(m.run)) {
			logger.Warningf("%s: %s; keeping this inbound on the direct path", i.Protocol, tproxy.FirewalldWarning)
		} else if err = tproxy.Acquire(owner, i.PoolCIDR, tproxy.Runner(m.run)); err != nil {
			logger.Warningf("%s: cannot enable Xray relay, keeping the direct path: %v", i.Protocol, err)
		} else {
			installed := make([]struct {
				table string
				rule  []string
			}, 0)
			for _, rr := range relayRules(i) {
				if err = m.ensure(bin, rr.table, rr.rule); err != nil {
					for j := len(installed) - 1; j >= 0; j-- {
						m.remove(bin, installed[j].table, installed[j].rule)
					}
					tproxy.Release(owner, tproxy.Runner(m.run))
					logger.Warningf("%s: cannot install Xray relay rules, keeping the direct path: %v", i.Protocol, err)
					break
				}
				installed = append(installed, rr)
			}
		}
	}
	m.states[i.ID] = i
	return nil
}
func (m *firewall) removeInstance(i Instance) {
	bin, err := m.iptables()
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rr := range relayRules(i) {
		m.remove(bin, rr.table, rr.rule)
	}
	if i.XrayRelayPort > 0 {
		tproxy.Release(string(i.Protocol)+":"+strconv.Itoa(i.ID), tproxy.Runner(m.run))
	}
	for _, r := range directRules(i) {
		table := ""
		if r[0] == "POSTROUTING" {
			table = "nat"
		}
		m.remove(bin, table, r)
	}
	for _, r := range inputRules(i) {
		m.remove(bin, "", r)
	}
	delete(m.states, i.ID)
}
