package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/engine"
)

// A missing engine binary must not take the companion down — pairing, status
// and revocation keep working — but the reason has to be reportable. In the
// Flatpak a silently unsupervised engine means torrents never play and the
// only clue was a Go transport error string in the browser console.
func TestSupervisorReportsMissingEngineBinary(t *testing.T) {
	sup, err := engine.New(engine.Config{
		Addr:       "127.0.0.1",
		Port:       18099,
		AutoStart:  true,
		BinaryPath: "crispy-no-such-engine-binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("Start must swallow a missing binary: %v", err)
	}
	defer sup.Stop()

	startErr := sup.StartError()
	if startErr == nil || !strings.Contains(startErr.Error(), "crispy-no-such-engine-binary") {
		t.Fatalf("StartError = %v, want it to name the missing engine binary", startErr)
	}
	if !strings.Contains(startErr.Error(), "not found") {
		t.Fatalf("StartError = %v, want a not-found reason", startErr)
	}
	if sup.Ready() {
		t.Fatal("supervisor reports ready with no engine running")
	}
}

// Explicitly disabling the engine is a configuration choice, not a failure to
// configure one — but it is still worth saying out loud in the log.
func TestSupervisorReportsDisabledAutoStart(t *testing.T) {
	sup, err := engine.New(engine.Config{Addr: "127.0.0.1", Port: 18099})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("Start must not fail when auto-start is off: %v", err)
	}
	defer sup.Stop()
	if err := sup.StartError(); err == nil {
		t.Fatal("StartError = nil, want an auto-start-off reason")
	}
}

// The engine cache directory must be created before spawning: TorrServer exits
// on a missing <path>/config.db, which is precisely the fresh-install state.
func TestSupervisorCreatesEngineCacheDir(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache", "torrserver")
	sup, err := engine.New(engine.Config{
		Addr:       "127.0.0.1",
		Port:       18099,
		AutoStart:  true,
		CacheDir:   cacheDir,
		BinaryPath: "crispy-no-such-engine-binary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("Start must not fail on a missing cache dir: %v", err)
	}
	defer sup.Stop()
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("engine cache dir not created: %v", err)
	}
}
