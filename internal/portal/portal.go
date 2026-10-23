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
	"strings"
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
//
// commandline is what the desktop writes into the login-autostart entry
// (spec: without it the Exec key of the app's desktop file is used — for
// the Flatpak build that is the start-menu entry with --menu-launch, not
// the plain serve command, so it must always be passed explicitly, the
// way Pika Backup passes its daemon binary). dbusActivatable must stay
// false: the companion has no D-Bus activation service, and autostart
// through D-Bus would route the login start through the desktop file
// again.
//
// Pure (no I/O) so it is unit-testable without a bus.
func BackgroundOptions(reason string, autostart bool, commandline []string, dbusActivatable bool, handleToken string) map[string]dbus.Variant {
	o := map[string]dbus.Variant{
		"handle_token": dbus.MakeVariant(handleToken),
		"reason":       dbus.MakeVariant(reason),
		"autostart":    dbus.MakeVariant(autostart),
	}
	if len(commandline) > 0 {
		o["commandline"] = dbus.MakeVariant(commandline)
	}
	if dbusActivatable {
		o["dbus-activatable"] = dbus.MakeVariant(dbusActivatable)
	}
	return o
}

// RequestBackground asks the desktop to allow background running and,
// if autostart is true, login autostart (see BackgroundOptions for the
// commandline / dbusActivatable pair). parentWindow is "" (no window).
// It blocks up to timeout waiting for the Request Response signal.
func (c *Conn) RequestBackground(reason string, autostart bool, commandline []string, dbusActivatable bool, timeout time.Duration) (background, autostartGranted bool, err error) {
	token := fmt.Sprintf("crispy%d", time.Now().UnixNano())
	obj := c.conn.Object(busName, dbus.ObjectPath(objectPath))
	var handle dbus.ObjectPath
	if err := obj.Call(ifaceBg+".RequestBackground", 0, "", BackgroundOptions(reason, autostart, commandline, dbusActivatable, token)).Store(&handle); err != nil {
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

// SetStatus tells the desktop what the daemon is doing while it runs in
// the background (Background v2). GNOME/KDE show this message next to the
// app instead of treating the process as a stray to be killed — the same
// call Pika Backup's monitor makes at startup. Errors are the caller's to
// ignore: without the background permission the call is simply rejected.
func (c *Conn) SetStatus(message string) error {
	obj := c.conn.Object(busName, dbus.ObjectPath(objectPath))
	return obj.Call(ifaceBg+".SetStatus", 0, map[string]dbus.Variant{
		"message": dbus.MakeVariant(message),
	}).Err
}

// Unavailable reports whether err means the desktop portal is not there
// at all: the session bus answered, but nothing owns the portal name and
// it cannot be activated — a session without xdg-desktop-portal, a bare
// bus like CI's, or an old portal with no Background interface.
//
// Callers use it to tell "nobody can ask the user" (fall back to the
// plain autostart entry) from a request that failed or was dismissed
// (retry next start; never bypass the user's consent).
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	var derr dbus.Error
	if errors.As(err, &derr) && missingName(derr.Name) {
		return true
	}
	// godbus has returned the error both by value and by pointer across
	// versions; accept either.
	var pderr *dbus.Error
	if errors.As(err, &pderr) && pderr != nil && missingName(pderr.Name) {
		return true
	}
	// "The name is not activatable" comes from the bus itself for a name
	// with neither an owner nor a D-Bus activation service file.
	return strings.Contains(err.Error(), "not activatable")
}

func missingName(name string) bool {
	switch name {
	case "org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownObject":
		return true
	}
	return false
}

func variantBool(m map[string]dbus.Variant, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}
