package portal_test

import (
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
	o := portal.BackgroundOptions("Keep streaming available", true, "crispy1")
	if o["reason"].Value().(string) != "Keep streaming available" {
		t.Fatalf("reason: %+v", o["reason"])
	}
	if !o["autostart"].Value().(bool) {
		t.Fatal("autostart must be true")
	}
	if o["handle_token"].Value().(string) != "crispy1" {
		t.Fatalf("handle_token: %+v", o["handle_token"])
	}
}

// Dial must fail fast in CI (no session bus), never hang or panic.
func TestDialFailsWithoutBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent-bus-socket-xyz")
	if _, err := portal.Dial(); err == nil {
		t.Skip("unexpected session bus present; skipping negative test")
	}
}
