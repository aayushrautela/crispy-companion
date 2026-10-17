package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/config"
)

func TestDefaultsAreLoopback(t *testing.T) {
	cfg := config.Default()
	if cfg.ListenAddr != "127.0.0.1" || cfg.EngineAddr != "127.0.0.1" {
		t.Fatalf("defaults must be loopback: %+v", cfg)
	}
	if cfg.SharedMode != "off" {
		t.Fatalf("shared default must be off, got %q", cfg.SharedMode)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWildcardOriginRejected(t *testing.T) {
	cfg := config.Default()
	cfg.AllowedOrigins = []string{"*"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("wildcard origin must fail validation")
	}
}

func TestRawModeRequiresExplicitLANAddr(t *testing.T) {
	for _, addr := range []string{"", "0.0.0.0", "127.0.0.1", "::", "localhost"} {
		cfg := config.Default()
		cfg.SharedMode = "raw"
		cfg.SharedListenAddr = addr
		if err := cfg.Validate(); err == nil {
			t.Fatalf("raw mode with addr %q must fail validation", addr)
		}
	}
	cfg := config.Default()
	cfg.SharedMode = "raw"
	cfg.SharedListenAddr = "192.168.1.50"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("raw mode with LAN addr must validate: %v", err)
	}
}

func TestNonLoopbackEngineRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte("[engine]\naddr = \"0.0.0.0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Engine supervisor enforces loopback separately; config loader must
	// carry the value through so the supervisor can reject it.
	cfg, err := config.Load(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EngineAddr != "0.0.0.0" {
		t.Fatalf("engine addr not carried: %q", cfg.EngineAddr)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("CRISPY_COMPANION_LISTEN_PORT", "12470")
	t.Setenv("CRISPY_COMPANION_SERVER_URL", "https://api.example.test")
	cfg, err := config.Load(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenPort != 12470 {
		t.Fatalf("port override failed: %d", cfg.ListenPort)
	}
	if cfg.CrispyServerURL != "https://api.example.test" {
		t.Fatalf("server url override failed: %q", cfg.CrispyServerURL)
	}
}

// The pinned engine binary has to be the default. An empty binaryPath makes
// the supervisor skip spawning torrserver altogether, so the shipped Flatpak
// pairs fine and then cannot play a single torrent.
func TestDefaultStartsPinnedEngine(t *testing.T) {
	cfg := config.Default()
	if cfg.EngineBinaryPath != "torrserver" {
		t.Fatalf("engine binaryPath default = %q, want %q", cfg.EngineBinaryPath, "torrserver")
	}
	if !cfg.EngineAutoStart {
		t.Fatal("engine autoStart default = false, want true")
	}
}
