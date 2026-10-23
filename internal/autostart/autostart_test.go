package autostart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/autostart"
)

// The entry builders run everywhere, so the macOS plist and the Windows
// Run data are covered by Linux CI too.

func TestXDGEntryLaunchesWithAutostartRun(t *testing.T) {
	entry := autostart.XDGEntry("/opt/crispy companion/crispy-companion")
	if !strings.Contains(entry, `Exec="/opt/crispy companion/crispy-companion" --autostart-run`) {
		t.Fatalf("Exec must quote the path and pass the flag:\n%s", entry)
	}
	for _, want := range []string{"NoDisplay=true", "X-GNOME-Autostart-Delay=90", "X-GNOME-UsesNotifications=true"} {
		if !strings.Contains(entry, want) {
			t.Fatalf("entry missing %q:\n%s", want, entry)
		}
	}
}

func TestLaunchAgentPlist(t *testing.T) {
	plist := autostart.LaunchAgentPlist("/Applications/Crispy Companion/crispy-companion", "/Users/u/Library/Logs/Crispy Companion")
	for _, want := range []string{
		"<string>/Applications/Crispy Companion/crispy-companion</string>",
		"<string>--autostart-run</string>",
		"<string>io.crispy.Companion</string>",
		"<key>WorkingDirectory</key>\n    <string>/Applications/Crispy Companion</string>",
		"<string>/Users/u/Library/Logs/Crispy Companion/crispy-companion.log</string>",
		"<key>RunAtLoad</key>\n    <true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLaunchAgentPlistEscapesPaths(t *testing.T) {
	plist := autostart.LaunchAgentPlist("/Users/a&b/crispy-companion", "/tmp/logs")
	if strings.Contains(plist, "a&b") {
		t.Fatalf("& must be escaped in XML:\n%s", plist)
	}
	if !strings.Contains(plist, "a&amp;b") {
		t.Fatalf("escaped path missing:\n%s", plist)
	}
}

func TestRunKeyData(t *testing.T) {
	got := autostart.RunKeyData(`C:\Users\u\AppData\Local\Crispy Companion\crispy-companion.exe`)
	want := `"C:\Users\u\AppData\Local\Crispy Companion\crispy-companion.exe" --autostart-run`
	if got != want {
		t.Fatalf("Run data:\n got %q\nwant %q", got, want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := autostart.StatePath(dir)
	if filepath.Base(path) != "autostart.json" {
		t.Fatalf("state file name: %s", path)
	}
	if _, ok := autostart.ReadState(path); ok {
		t.Fatal("absent state must read as not present")
	}
	st := autostart.State{Enabled: true, Method: autostart.MethodRegistry, RequestedAt: time.Now().UTC().Truncate(time.Second)}
	if err := autostart.WriteState(path, st); err != nil {
		t.Fatalf("write state: %v", err)
	}
	got, ok := autostart.ReadState(path)
	if !ok || !got.Enabled || got.Method != autostart.MethodRegistry || !got.Settled() {
		t.Fatalf("state: %+v ok=%v", got, ok)
	}
}

func TestStateUnreadableIsUnsettled(t *testing.T) {
	dir := t.TempDir()
	path := autostart.StatePath(dir)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := autostart.ReadState(path); ok {
		t.Fatal("a corrupt state file must read as not present")
	}
}

func TestRefusedStateSettles(t *testing.T) {
	st := autostart.State{Refused: true}
	if !st.Settled() {
		t.Fatal("a refusal must settle the question")
	}
	if (autostart.State{}).Settled() {
		t.Fatal("an empty state must stay unsettled")
	}
}
