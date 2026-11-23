package l2tp

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HookIncidentError marks a durable hook observation or an interrupted
// incident write. Its removal alone does not prove final traffic was accounted.
type HookIncidentError struct{ Path string }

func (e *HookIncidentError) Error() string {
	return fmt.Sprintf("l2tp: hook incident %q requires manual reconciliation", e.Path)
}

// HookPendingError can be a live ip-up hook or an interrupted one. While it
// exists, sensitive operations and polling must wait rather than infer success.
type HookPendingError struct{ Path string }

func (e *HookPendingError) Error() string {
	return fmt.Sprintf("l2tp: hook operation pending at %q", e.Path)
}

// CheckHookIncidents must not inspect or remove incident contents. Even an
// incomplete file is evidence that a hook started and may have been interrupted.
func CheckHookIncidents() error {
	root, err := os.OpenRoot(l2tpRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("l2tp: open hook incident root: %w", err)
	}
	defer root.Close()
	entries, err := readEvidenceDir(root, "hook-incidents")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("l2tp: inspect hook incidents: %w", err)
	}
	var pending string
	for _, entry := range entries {
		path := filepath.Join(l2tpRoot(), "hook-incidents", entry.Name())
		if strings.HasPrefix(entry.Name(), "pending.") {
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("l2tp: inspect pending hook evidence %q: %w", path, err)
			}
			if info.Mode().IsRegular() {
				pending = path
				continue
			}
		}
		return &HookIncidentError{Path: path}
	}
	if pending != "" {
		return &HookPendingError{Path: pending}
	}
	return nil
}

// CheckClientSessionEvidence fences removal when a local L2TP ip-up marker
// names one of the clients, even if its database attachment has drifted away.
// Malformed or unreadable evidence is not assumed unrelated. An empty scan is
// not proof of disconnection or complete accounting: hooks are not fenced by
// this check and may never have run. scope nil checks all inbound IDs.
func CheckClientSessionEvidence(emails []string, scope []int) error {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(emails))
	for _, email := range emails {
		if key := strings.ToLower(strings.TrimSpace(email)); key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	// A hook incident can contain usage for an unknown identity and is
	// therefore a global fence, not merely a per-inbound marker.
	if err := CheckHookIncidents(); err != nil {
		return err
	}
	allowed := make(map[int]bool, len(scope))
	for _, id := range scope {
		allowed[id] = true
	}
	root, err := os.OpenRoot(l2tpRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("l2tp: open runtime root: %w", err)
	}
	defer root.Close()
	groups, err := readEvidenceDir(root, ".")
	if err != nil {
		return fmt.Errorf("l2tp: inspect runtime root: %w", err)
	}
	for _, group := range groups {
		if group.Name() == "hook-incidents" {
			if !group.IsDir() {
				return fmt.Errorf("l2tp: invalid hook incident directory")
			}
			if err := CheckHookIncidents(); err != nil {
				return err
			}
			continue
		}
		id, parseErr := strconv.Atoi(group.Name())
		if parseErr != nil || id <= 0 || group.Name() != strconv.Itoa(id) || !group.IsDir() {
			return fmt.Errorf("l2tp: unexpected runtime entry %q requires manual review", group.Name())
		}
		if scope != nil && !allowed[id] {
			continue
		}
		dir := filepath.Join(group.Name(), "sessions")
		entries, err := readEvidenceDir(root, dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("l2tp: inspect session markers for inbound %d: %w", id, err)
		}
		for _, entry := range entries {
			if !validInterfaceName(entry.Name()) || entry.IsDir() {
				return fmt.Errorf("l2tp: invalid session marker %q requires manual review", entry.Name())
			}
			path := filepath.Join(dir, entry.Name())
			email, err := readEvidenceEmail(root, path)
			if err != nil {
				return err
			}
			email = strings.ToLower(email)
			if wanted[email] {
				return fmt.Errorf("l2tp: session evidence for inbound %d requires manual reconciliation before removal", id)
			}
		}
	}
	return nil
}

func readEvidenceDir(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := openEvidenceDir(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// readEvidenceEmail accepts only the single bounded line written by the hook.
func readEvidenceEmail(root *os.Root, path string) (string, error) {
	f, err := openEvidenceRecord(root, path)
	if err != nil {
		return "", fmt.Errorf("l2tp: inspect session marker %q: %w", path, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return "", fmt.Errorf("l2tp: read session marker %q: %w", path, errors.Join(readErr, closeErr))
	}
	if len(data) > 4096 || len(data) == 0 || data[len(data)-1] != '\n' || strings.Count(string(data), "\n") != 1 || !utf8.Valid(data) {
		return "", fmt.Errorf("l2tp: malformed session marker %q requires manual review", path)
	}
	for _, r := range string(data[:len(data)-1]) {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("l2tp: invalid session identity in marker %q requires manual review", path)
		}
	}
	email := strings.TrimSpace(string(data))
	if email == "" {
		return "", fmt.Errorf("l2tp: empty session marker %q requires manual review", path)
	}
	return email, nil
}
