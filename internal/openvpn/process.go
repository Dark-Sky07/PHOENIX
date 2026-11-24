package openvpn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/logger"
)

// openvpnBinDir resolves the panel's binary directory to an absolute path.
// The default XUI_BIN_FOLDER is "bin", but systemd's working directory and
// manual invocations are not guaranteed to be the same. OpenVPN config paths
// must not silently become relative to /root (or another caller directory).
func openvpnBinDir() string {
	dir := config.GetBinFolderPath()
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), dir)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// openvpnDir is the shared root for all openvpn inbound data directories.
func openvpnDir() string {
	return filepath.Join(openvpnBinDir(), "openvpn")
}

// GetBinaryPath locates the openvpn binary: a binary dropped next to the
// panel's own binaries (same convention as xray/mtg) wins, then the system
// PATH. Returns "" when neither exists — the panel degrades gracefully in
// that case (certs/profiles still work; daemons simply do not run).
func GetBinaryPath() string {
	name := "openvpn"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	local := filepath.Join(openvpnBinDir(), name)
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return ""
}

// Available reports whether the openvpn binary can be found at all.
func Available() bool {
	return GetBinaryPath() != ""
}

var (
	gracefulStopTimeout = 5 * time.Second
	forceStopTimeout    = 2 * time.Second
)

// procLogWriter consumes the openvpn daemon's stdout/stderr, forwards each
// line to the x-ui log and remembers the most recent line for diagnostics.
type procLogWriter struct {
	mu       sync.Mutex
	label    string
	buf      string
	lastLine string
}

func (w *procLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf += string(p)
	for {
		i := strings.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i]
		w.buf = w.buf[i+1:]
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed != "" {
			w.lastLine = trimmed
			logger.Infof("openvpn: %s | %s", w.label, trimmed)
		}
	}
	return len(p), nil
}

func (w *procLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf != "" {
		trimmed := strings.TrimSpace(strings.TrimRight(w.buf, "\r"))
		if trimmed != "" {
			w.lastLine = trimmed
			logger.Infof("openvpn: %s | %s", w.label, trimmed)
		}
		w.buf = ""
	}
}

func (w *procLogWriter) LastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastLine
}

// Process wraps a single openvpn daemon for one inbound.
type Process struct {
	cmd             *exec.Cmd
	done            chan struct{}
	configPath      string
	logWriter       *procLogWriter
	exitErr         error
	intentionalStop atomic.Bool
}

func newProcess(configPath, label string) *Process {
	return &Process{
		configPath: configPath,
		logWriter:  &procLogWriter{label: label},
	}
}

// IsRunning reports whether the daemon process is currently running.
func (p *Process) IsRunning() bool {
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	if p.done != nil {
		select {
		case <-p.done:
			return false
		default:
		}
	}
	return p.cmd.ProcessState == nil
}

// GetResult returns the last log line or the exit error from the daemon.
func (p *Process) GetResult() string {
	if line := p.logWriter.LastLine(); line != "" {
		return line
	}
	if p.exitErr != nil {
		return p.exitErr.Error()
	}
	return ""
}

// Start launches the daemon against its generated config file. A retained PID
// file is not proof of ownership or final accounting: refuse to start rather
// than signal an unverified process or overwrite the evidence.
func (p *Process) Start() error {
	if p.IsRunning() {
		return errors.New("openvpn is already running")
	}
	binary := GetBinaryPath()
	if binary == "" {
		return errors.New("openvpn binary not found (install openvpn or drop an 'openvpn' binary next to the x-ui binary)")
	}
	pidPath := filepath.Join(filepath.Dir(p.configPath), "openvpn.pid")
	if _, err := os.Lstat(pidPath); err == nil {
		return fmt.Errorf("openvpn: retained PID evidence at %s requires manual reconciliation", pidPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("openvpn: inspect PID evidence: %w", err)
	}
	cmd := exec.Command(binary, "--config", p.configPath)
	cmd.Stdout = p.logWriter
	cmd.Stderr = p.logWriter
	p.cmd = cmd
	p.done = make(chan struct{})
	p.exitErr = nil
	p.intentionalStop.Store(false)
	if err := cmd.Start(); err != nil {
		close(p.done)
		p.cmd = nil
		return err
	}
	go p.wait(cmd)
	return nil
}

func (p *Process) wait(cmd *exec.Cmd) {
	defer close(p.done)
	err := cmd.Wait()
	p.logWriter.Flush()
	if err == nil || p.intentionalStop.Load() {
		return
	}
	logger.Errorf("openvpn: daemon process exited: %v", err)
	p.exitErr = err
}

// Stop terminates the running daemon gracefully, falling back to a kill.
func (p *Process) Stop() error {
	if !p.IsRunning() {
		return nil
	}
	p.intentionalStop.Store(true)

	if runtime.GOOS == "windows" {
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-p.done
		return p.removeOwnedPIDFile()
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-p.done:
	case <-time.After(gracefulStopTimeout):
	}
	if p.IsRunning() {
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(forceStopTimeout):
		}
	}
	select {
	case <-p.done:
		return p.removeOwnedPIDFile()
	default:
		return errors.New("openvpn: daemon termination not confirmed; PID evidence retained")
	}
}

// removeOwnedPIDFile removes only the PID file of the process this Process
// started, after its exit has been observed. External writers and other panel
// processes are not fenced by this process-local check.
func (p *Process) removeOwnedPIDFile() error {
	path := filepath.Join(filepath.Dir(p.configPath), "openvpn.pid")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("openvpn: unexpected PID file type; manual reconciliation required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != strconv.Itoa(p.cmd.Process.Pid) {
		return errors.New("openvpn: PID file does not match stopped daemon; manual reconciliation required")
	}
	return os.Remove(path)
}

// FreeLocalPort allocates an ephemeral loopback TCP port for the daemon's
// management interface.
func FreeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
