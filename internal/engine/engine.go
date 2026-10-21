// Package engine supervises the embedded TorrServer child process.
//
// M1 scope: lifecycle only (spawn if a binary is configured, monitor,
// report readiness). The TorrServer API mapping (/torrents, /stream,
// /cache/state) lands in M3 behind the Client seam below.
//
// Invariant (PLAN §4): the engine is always bound to loopback. This
// package refuses to start an engine configured with a non-loopback addr.
package engine

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Config is the engine subset of the companion config.
type Config struct {
	Addr          string
	Port          int
	AutoStart     bool
	BinaryPath    string
	CacheDir      string
	MaxCacheBytes int64
}

// Supervisor owns the child process.
type Supervisor struct {
	cfg Config
	mu  sync.Mutex
	cmd *exec.Cmd
	ok  bool
	// startErr remembers why the engine could not be started so the caller
	// can log it: auto-start failures are non-fatal by design.
	startErr error
}

// New creates a supervisor. It does not start anything.
func New(cfg Config) (*Supervisor, error) {
	if cfg.Addr != "127.0.0.1" && cfg.Addr != "localhost" {
		return nil, fmt.Errorf("engine addr must be loopback, got %q", cfg.Addr)
	}
	return &Supervisor{cfg: cfg}, nil
}

// Start spawns the engine if AutoStart and a binary is configured.
// With no binary (dev / M1), it reports not-ready without error.
func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.cfg.AutoStart || s.cfg.BinaryPath == "" {
		s.ok = false
		s.startErr = errors.New("engine auto-start is off (engine.autoStart / engine.binaryPath)")
		return nil
	}
	args := []string{
		"--ip", s.cfg.Addr,
		"--port", fmt.Sprint(s.cfg.Port),
	}
	if s.cfg.CacheDir != "" {
		// TorrServer opens <path>/config.db on startup and exits if the
		// directory is missing — which is exactly the state of a fresh
		// install, where the companion's own data dir exists but nothing has
		// created the cache subdirectory yet. Create it here so the engine
		// starts on first run instead of dying silently.
		if err := os.MkdirAll(s.cfg.CacheDir, 0o700); err != nil {
			s.ok = false
			s.startErr = fmt.Errorf("create engine cache dir %q: %w", s.cfg.CacheDir, err)
			return nil
		}
		args = append(args, "--path", s.cfg.CacheDir)
	}
	if s.cfg.MaxCacheBytes > 0 {
		args = append(args, "--maxsize", fmt.Sprint(s.cfg.MaxCacheBytes))
	}
	cmd := exec.Command(s.cfg.BinaryPath, args...)
	inheritEngineOutput(cmd)
	if err := cmd.Start(); err != nil {
		// A missing binary (exec.ErrNotFound for a bare name resolved
		// through PATH, os.ErrNotExist for an absolute path) is the common
		// case; a wrong-arch or corrupt binary shows up here too. None of it
		// is worth killing the companion over: pairing, status and revocation
		// keep working, StartError() tells the log why torrents cannot play.
		s.ok = false
		s.startErr = fmt.Errorf("start engine %q: %w", s.cfg.BinaryPath, err)
		return nil
	}
	s.cmd = cmd
	go s.watch(cmd)
	s.ok = true
	s.startErr = nil
	return nil
}

// inheritEngineOutput wires the engine's stdout/stderr to the companion's own
// when the companion itself runs on a terminal, so a foreground launch shows
// what TorrServer says (tracker and peer activity is the only way to tell a
// dead torrent from a blocked one). CRISPY_COMPANION_ENGINE_LOG=1 forces it
// on for non-terminal runs, e.g. `flatpak run … | tee log`.
//
// Everywhere else the engine stays quiet: a desktop launch has no terminal,
// and a child writing to a closed pipe would die from SIGPIPE.
func inheritEngineOutput(cmd *exec.Cmd) {
	if os.Getenv("CRISPY_COMPANION_ENGINE_LOG") == "1" {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return
	}
	if isTerminal(os.Stdout) || isTerminal(os.Stderr) {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
}

// isTerminal reports whether f is attached to a character device (a tty or
// pty). Pipes and files are not.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// StartError reports why the engine could not be started, or nil when it
// started (or is still coming up). Start deliberately swallows a missing
// binary so the rest of the companion keeps working; this is how that reason
// reaches the log instead of the void.
func (s *Supervisor) StartError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startErr
}

func (s *Supervisor) watch(cmd *exec.Cmd) {
	_ = cmd.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == cmd {
		s.cmd = nil
		s.ok = false
	}
}

// Ready reports whether the engine TCP port currently accepts connections.
func (s *Supervisor) Ready() bool {
	addr := net.JoinHostPort(s.cfg.Addr, fmt.Sprint(s.cfg.Port))
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Stop terminates the child if running.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.cmd = nil
	s.ok = false
}
