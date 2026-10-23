package portal_test

import (
	"errors"
	"testing"

	"github.com/crispy-streaming/crispy-companion/internal/portal"
	"github.com/godbus/dbus/v5"
)

func TestNotificationPayload(t *testing.T) {
	p := portal.NotificationPayload("Paired", "Paired for alice@example.com",
		[]portal.Button{{Label: "Revoke", Action: "revoke"}})
	if v, ok := p["title"]; !ok || v.Value().(string) != "Paired" {
		t.Fatalf("title: %+v", p["title"])
	}
	if v, ok := p["body"]; !ok || v.Value().(string) != "Paired for alice@example.com" {
		t.Fatalf("body: %+v", p["body"])
	}
	raw, ok := p["buttons"]
	if !ok {
		t.Fatal("buttons missing")
	}
	btns, ok := raw.Value().([]map[string]dbus.Variant)
	if !ok || len(btns) != 1 {
		t.Fatalf("buttons shape: %T", raw.Value())
	}
	if btns[0]["label"].Value().(string) != "Revoke" || btns[0]["action"].Value().(string) != "revoke" {
		t.Fatalf("button: %+v", btns[0])
	}
}

func TestNotificationPayloadNoButtons(t *testing.T) {
	p := portal.NotificationPayload("T", "B", nil)
	if _, ok := p["buttons"]; ok {
		t.Fatal("buttons key must be absent when empty")
	}
}

func TestBackgroundOptions(t *testing.T) {
	o := portal.BackgroundOptions("Keep streaming available", true, []string{"crispy-companion"}, false, "crispy1")
	if o["reason"].Value().(string) != "Keep streaming available" {
		t.Fatalf("reason: %+v", o["reason"])
	}
	if !o["autostart"].Value().(bool) {
		t.Fatal("autostart must be true")
	}
	if o["handle_token"].Value().(string) != "crispy1" {
		t.Fatalf("handle_token: %+v", o["handle_token"])
	}
	// The autostart entry must pin the plain serve command: without it
	// the portal falls back to the desktop file's Exec (--menu-launch).
	cmd, ok := o["commandline"].Value().([]string)
	if !ok || len(cmd) != 1 || cmd[0] != "crispy-companion" {
		t.Fatalf("commandline: %+v", o["commandline"])
	}
	if _, ok := o["dbus-activatable"]; ok {
		t.Fatal("dbus-activatable must be absent when false")
	}
}

func TestBackgroundOptionsWithoutCommandline(t *testing.T) {
	o := portal.BackgroundOptions("r", false, nil, true, "t")
	if _, ok := o["commandline"]; ok {
		t.Fatal("commandline must be absent when empty")
	}
	if !o["dbus-activatable"].Value().(bool) {
		t.Fatal("dbus-activatable must be true")
	}
}

// Dial must fail fast in CI (no session bus), never hang or panic.
func TestDialFailsWithoutBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent-bus-socket-xyz")
	if _, err := portal.Dial(); err == nil {
		t.Skip("unexpected session bus present; skipping negative test")
	}
}

// Unavailable: the bus answered but no portal is behind the name.
func TestUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"service unknown", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{"The name is not activatable"}}, true},
		{"no owner", dbus.Error{Name: "org.freedesktop.DBus.Error.NameHasNoOwner"}, true},
		{"no background interface", dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownMethod"}, true},
		{"not activatable, plain text", errors.New("The name is not activatable"), true},
		{"cancelled by the user", dbus.Error{Name: "org.freedesktop.portal.Error.Cancelled"}, false},
		{"timeout", errors.New("portal: background request timed out"), false},
	}
	for _, c := range cases {
		if got := portal.Unavailable(c.err); got != c.want {
			t.Errorf("%s: Unavailable = %v, want %v", c.name, got, c.want)
		}
	}
}
