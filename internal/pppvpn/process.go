package pppvpn

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
)

var (
	pppGracefulStopTimeout = 5 * time.Second
	pppForceStopTimeout    = 2 * time.Second
)

type lineLogger struct {
	mu       sync.Mutex
	protocol Protocol
	buf      string
	last     string
}

func (w *lineLogger) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	for {
		n := strings.IndexByte(w.buf, '\n')
		if n < 0 {
			break
		}
		line := strings.TrimSpace(w.buf[:n])
		w.buf = w.buf[n+1:]
		if line != "" {
			w.last = line
			logger.Infof("%s: %s", w.protocol, line)
		}
	}
	return len(p), nil
}
func (w *lineLogger) Last() string { w.mu.Lock(); defer w.mu.Unlock(); return w.last }

type Process struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	log  *lineLogger
}

func daemonPath(protocol Protocol) (string, error) {
	name := "pptpd"
	if protocol == SSTP {
		name = "sstpd"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s daemon not found; install %s", protocol, name)
	}
	return path, nil
}

func (p *Process) Start(i Instance, configPath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.runningLocked() {
		return errors.New("pppvpn daemon is already running")
	}
	bin, err := daemonPath(i.Protocol)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if i.Protocol == PPTP {
		cmd = exec.Command(bin, "--fg", "--conf", configPath)
	} else {
		cmd = exec.Command(bin, "--conf-file", configPath, "--conf-section", "server")
	}
	p.log = &lineLogger{protocol: i.Protocol}
	cmd.Stdout = p.log
	cmd.Stderr = p.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", i.Protocol, err)
	}
	p.cmd, p.done = cmd, make(chan struct{})
	done := p.done
	go func() {
		err := cmd.Wait()
		if err != nil {
			logger.Warningf("%s daemon exited: %v (%s)", i.Protocol, err, p.log.Last())
		}
		close(done)
	}()
	select {
	case <-done:
		line := p.log.Last()
		if line == "" {
			line = "process exited during startup"
		}
		return fmt.Errorf("%s daemon failed during startup: %s", i.Protocol, line)
	case <-time.After(1200 * time.Millisecond):
		return nil
	}
}

func (p *Process) runningLocked() bool {
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	if p.done == nil {
		return true
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
func (p *Process) IsRunning() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.runningLocked() }

// Stop waits for the supervised daemon's Wait goroutine, not merely for a
// signal to be sent. This only covers the parent process; it is not evidence
// that every pppd child, session, counter writer or traffic checkpoint drained.
func (p *Process) Stop() error {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if done == nil {
		return errors.New("pppvpn: cannot verify daemon exit without a wait signal")
	}
	select {
	case <-done:
		return nil
	default:
	}
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
		return nil
	case <-time.After(pppGracefulStopTimeout):
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
		return nil
	case <-time.After(pppForceStopTimeout):
		return errors.New("pppvpn: daemon exit was not observed after forced stop")
	}
}

func processConfigPath(i Instance) string {
	name := "pptpd.conf"
	if i.Protocol == SSTP {
		name = "sstpd.conf"
	}
	return filepath.Join(dirFor(i), name)
}
