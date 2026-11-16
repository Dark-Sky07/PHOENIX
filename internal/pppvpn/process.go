package pppvpn

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
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

func stopOrphan(protocol Protocol, configPath string) {
	if runtime.GOOS != "linux" {
		return
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	name := "pptpd"
	if protocol == SSTP {
		name = "sstpd"
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		line := string(cmdline)
		if !strings.Contains(line, name) || !strings.Contains(line, configPath) {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = process.Signal(syscall.SIGTERM)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if process.Signal(syscall.Signal(0)) != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if process.Signal(syscall.Signal(0)) == nil {
			_ = process.Kill()
		}
	}
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
	stopOrphan(i.Protocol, configPath)
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

func (p *Process) Stop() error {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil || done == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

func processConfigPath(i Instance) string {
	name := "pptpd.conf"
	if i.Protocol == SSTP {
		name = "sstpd.conf"
	}
	return filepath.Join(dirFor(i), name)
}
