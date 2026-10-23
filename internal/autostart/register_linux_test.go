//go:build linux

package autostart_test

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/autostart"
)

// Register must write the user-scope XDG entry under $XDG_CONFIG_HOME.
func TestRegisterWritesXDGEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	exe := filepath.Join(dir, "bin", "crispy-companion")

	method, err := autostart.Register(exe, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if method != autostart.MethodXDG {
		t.Fatalf("method: %s", method)
	}
	b, err := os.ReadFile(filepath.Join(dir, "autostart", autostart.EntryName+".desktop"))
	if err != nil {
		t.Fatalf("entry not written: %v", err)
	}
	if !strings.Contains(string(b), `Exec="`+exe+`" --autostart-run`) {
		t.Fatalf("entry content:\n%s", b)
	}
}
