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
		return nil
	}
	args := []string{
		"--ip", s.cfg.Addr,
		"--port", fmt.Sprint(s.cfg.Port),
	}
	if s.cfg.CacheDir != "" {
		args = append(args, "--path", s.cfg.CacheDir)
	}
	if s.cfg.MaxCacheBytes > 0 {
		args = append(args, "--maxsize", fmt.Sprint(s.cfg.MaxCacheBytes))
	}
	cmd := exec.Command(s.cfg.BinaryPath, args...)
	cmd.Stdout = nil // M1: inherit nothing; M3 wires ring-buffer logs.
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.ok = false
			return nil
		}
		return fmt.Errorf("start engine: %w", err)
	}
	s.cmd = cmd
	go s.watch(cmd)
	s.ok = true
	return nil
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
