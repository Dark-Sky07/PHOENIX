package openvpn

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// managementKill sends the numeric client ID obtained from a parsed status
// response. A common name is never interpolated into a management command.
// SUCCESS acknowledges the command, not immediate termination; callers must
// confirm disappearance from a subsequent status poll.
func managementKill(mgmtPort int, id string) error {
	if mgmtPort <= 0 || id == "" || strings.Trim(id, "0123456789") != "" {
		return errors.New("openvpn: invalid client-kill request")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", mgmtPort), 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("client-kill " + id + "\n")); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "SUCCESS:"):
			return nil
		case strings.HasPrefix(line, "ERROR:"):
			return fmt.Errorf("openvpn: client-kill rejected: %s", line)
		case line == "END":
			return errors.New("openvpn: client-kill acknowledgement missing")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("openvpn: incomplete client-kill response")
}

// RevokeClientEverywhere applies revocation to every daemon generation the
// manager currently owns. Generations from a previous panel process or daemon
// are not guessed: an operator must reconcile those separately.
func (m *Manager) RevokeClientEverywhere(email string) error {
	if !sessionCommonRegex.MatchString(email) {
		return errors.New("openvpn: invalid revocation identity")
	}
	m.mu.Lock()
	seen := make(map[string]bool)
	var generations []string
	for _, man := range m.procs {
		if man.generation != "" && !seen[man.generation] {
			seen[man.generation] = true
			generations = append(generations, man.generation)
		}
	}
	m.mu.Unlock()
	if len(generations) == 0 {
		return errors.New("openvpn: no managed daemon generation available for revocation")
	}
	var joined error
	for _, generation := range generations {
		if err := m.RevokeClient(email, generation); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// RevokeClient removes the certificate digest from the authentication
// allowlist and terminates current sessions for that common name. It requires
// the daemon generation observed by the caller; a stale generation is refused
// because client IDs are only meaningful within one daemon epoch. A successful
// return requires status confirmation that no session with a killed ID remains.
// It does not prove that traffic after the last poll was accounted for, and a
// caller must not release quota unless that accounting is independently proven.
func (m *Manager) RevokeClient(email string, generation string) error {
	if !sessionCommonRegex.MatchString(email) || !generationPattern.MatchString(generation) {
		return errors.New("openvpn: invalid revocation identity or generation")
	}
	type target struct {
		id      int
		port    int
		clients []string
	}
	m.mu.Lock()
	var targets []target
	for id, man := range m.procs {
		if man.generation != generation || man.mgmtPort <= 0 {
			continue
		}
		found := false
		for _, cn := range man.clients {
			if cn == email {
				found = true
			}
		}
		if found {
			clients := append([]string(nil), man.clients...)
			targets = append(targets, target{id: id, port: man.mgmtPort, clients: clients})
		}
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return errors.New("openvpn: no daemon in the requested generation serves that client")
	}

	// Persist the deny decision before touching the generated allowlist, so a
	// restart or re-attachment cannot re-authorize the same certificate.
	for _, t := range targets {
		dir := dataDirForID(t.id)
		digest, err := certificateDigestForClient(dir, email)
		if err != nil {
			return fmt.Errorf("inbound %d: %w", t.id, err)
		}
		if err := recordRevokedDigest(dir, digest); err != nil {
			return fmt.Errorf("inbound %d: persist revocation: %w", t.id, err)
		}
	}
	// Fail closed for new authentication before touching active sessions.
	for _, t := range targets {
		remaining := make([]string, 0, len(t.clients))
		for _, cn := range t.clients {
			if cn != email {
				remaining = append(remaining, cn)
			}
		}
		if err := writeClientAllowlist(dataDirForID(t.id), remaining); err != nil {
			return fmt.Errorf("inbound %d: update revocation state: %w", t.id, err)
		}
	}

	var joined error
	for _, t := range targets {
		list, err := clientList(t.port)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("inbound %d: status before revocation: %w", t.id, err))
			continue
		}
		var killed []string
		for cn, counters := range list {
			if cn != email {
				continue
			}
			if counters.ID == "" || strings.Trim(counters.ID, "0123456789") != "" {
				joined = errors.Join(joined, fmt.Errorf("inbound %d: client ID missing before revocation", t.id))
				continue
			}
			if err := managementKill(t.port, counters.ID); err != nil {
				joined = errors.Join(joined, fmt.Errorf("inbound %d: kill session %s: %w", t.id, counters.ID, err))
			} else {
				killed = append(killed, counters.ID)
			}
		}
		if len(killed) == 0 {
			joined = errors.Join(joined, fmt.Errorf("inbound %d: no active session was killed", t.id))
			continue
		}
		// A kill acknowledgement is not proof of removal. Confirm against a
		// fresh management snapshot instead of relying on the callback.
		confirmed := false
		for attempt := 0; attempt < 10; attempt++ {
			current, err := clientList(t.port)
			if err != nil {
				break
			}
			still := false
			for cn, counters := range current {
				if cn != email {
					continue
				}
				for _, id := range killed {
					if counters.ID == id {
						still = true
					}
				}
			}
			if !still {
				confirmed = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !confirmed {
			joined = errors.Join(joined, fmt.Errorf("inbound %d: session termination unconfirmed; do not release quota", t.id))
		}
	}
	return joined
}
