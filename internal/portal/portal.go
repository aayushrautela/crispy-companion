// Package portal speaks the XDG Desktop Portal D-Bus APIs the companion
// needs on Linux: Notification (confirmation + Revoke button) and
// Background (login autostart request).
//
// Spec sources (fetched 2026-10-14, xdg-desktop-portal docs):
//   - org.freedesktop.portal.Notification v2: AddNotification(id: s,
//     notification: a{sv}); buttons aa{sv} with label (s, mandatory) +
//     action (s, mandatory). Non-"app." actions are delivered back to us
//     as the ActionInvoked(id: s, action: s, parameter: av) signal.
//   - org.freedesktop.portal.Background v2: RequestBackground(parent: s,
//     options: a{sv}) -> handle (o); result via
//     org.freedesktop.portal.Request.Response(response: u, results: a{sv})
//     with background (b) + autostart (b).
//
// Both live on bus name org.freedesktop.portal.Desktop, object path
// /org/freedesktop/portal/desktop. There is intentionally no build tag:
// off-Linux (or bus-less) Dial simply fails and callers fall back to the
// log notifier. Live portal behavior can only be verified on a real
// desktop (M4 acceptance: flatpak run host); the payload builders below
// are pure and unit-tested.
package portal

import (
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.freedesktop.portal.Desktop"
	objectPath = "/org/freedesktop/portal/desktop"
	ifaceNotif = "org.freedesktop.portal.Notification"
	ifaceBg    = "org.freedesktop.portal.Background"
	ifaceReq   = "org.freedesktop.portal.Request"
)

// Button is a notification button. Action must NOT start with "app." —
// such actions go through D-Bus activation instead of our ActionInvoked
// subscription and would silently do nothing.
type Button struct {
	Label  string
	Action string
}

// Conn is a session-bus connection to the portals.
type Conn struct {
	conn *dbus.Conn
}

// Dial connects to the session bus. It fails fast off-desktop (no bus).
func Dial() (*Conn, error) {
	c, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &Conn{conn: c}, nil
}

// Close releases the bus connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// NotificationPayload builds the a{sv} vardict for AddNotification.
// Pure (no I/O) so it is unit-testable without a bus.
func NotificationPayload(title, body string, buttons []Button) map[string]dbus.Variant {
	p := map[string]dbus.Variant{
		"title": dbus.MakeVariant(title),
		"body":  dbus.MakeVariant(body),
	}
	if len(buttons) > 0 {
		btns := make([]map[string]dbus.Variant, 0, len(buttons))
		for _, b := range buttons {
			btns = append(btns, map[string]dbus.Variant{
				"label":  dbus.MakeVariant(b.Label),
				"action": dbus.MakeVariant(b.Action),
			})
		}
		p["buttons"] = dbus.MakeVariant(btns)
	}
	return p
}

// Notify posts (or updates, by id) a desktop notification.
func (c *Conn) Notify(id, title, body string, buttons []Button) error {
	obj := c.conn.Object(busName, dbus.ObjectPath(objectPath))
	call := obj.Call(ifaceNotif+".AddNotification", 0, id, NotificationPayload(title, body, buttons))
	return call.Err
}

// OnAction subscribes to ActionInvoked and invokes fn for every
// non-exported action activation. It runs until the connection closes;
// callers typically launch it once at startup.
func (c *Conn) OnAction(fn func(notificationID, action string)) error {
	if err := c.conn.AddMatchSignal(dbus.WithMatchInterface(ifaceNotif), dbus.WithMatchMember("ActionInvoked")); err != nil {
		return err
	}
	ch := make(chan *dbus.Signal, 8)
	c.conn.Signal(ch)
	go func() {
		for sig := range ch {
			if len(sig.Body) < 2 {
				continue
			}
			id, _ := sig.Body[0].(string)
			action, _ := sig.Body[1].(string)
			if id == "" || action == "" {
				continue
			}
			fn(id, action)
		}
	}()
	return nil
}

// BackgroundOptions builds the a{sv} options for RequestBackground.
// Pure (no I/O) so it is unit-testable without a bus.
func BackgroundOptions(reason string, autostart bool, handleToken string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(handleToken),
		"reason":       dbus.MakeVariant(reason),
		"autostart":    dbus.MakeVariant(autostart),
	}
}

// RequestBackground asks the desktop to allow background running and,
// if autostart is true, login autostart. parentWindow is "" (no window).
// It blocks up to timeout waiting for the Request Response signal.
func (c *Conn) RequestBackground(reason string, autostart bool, timeout time.Duration) (background, autostartGranted bool, err error) {
	token := fmt.Sprintf("crispy%d", time.Now().UnixNano())
	obj := c.conn.Object(busName, dbus.ObjectPath(objectPath))
	var handle dbus.ObjectPath
	if err := obj.Call(ifaceBg+".RequestBackground", 0, "", BackgroundOptions(reason, autostart, token)).Store(&handle); err != nil {
		return false, false, err
	}
	ch := make(chan *dbus.Signal, 4)
	if err := c.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(handle),
		dbus.WithMatchInterface(ifaceReq),
		dbus.WithMatchMember("Response"),
	); err != nil {
		return false, false, err
	}
	c.conn.Signal(ch)
	defer c.conn.RemoveSignal(ch)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case sig := <-ch:
			if sig.Path != handle || len(sig.Body) < 2 {
				continue
			}
			results, _ := sig.Body[1].(map[string]dbus.Variant)
			if results == nil {
				return false, false, errors.New("portal: malformed Response")
			}
			return variantBool(results, "background"), variantBool(results, "autostart"), nil
		case <-timer.C:
			return false, false, errors.New("portal: background request timed out")
		}
	}
}

func variantBool(m map[string]dbus.Variant, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}
