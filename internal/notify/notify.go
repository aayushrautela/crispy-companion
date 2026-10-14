// Package notify posts desktop notifications.
//
// M1 scope: log-backed stub with the M2/M4 seam (Notify + Revoke action).
// Platform implementations (Flatpak portal, Windows toast, macOS
// UNUserNotificationCenter) land in M4; the pairing flow must not depend
// on action-button delivery, only on this confirmation path.
package notify

import (
	"log"
)

// Action is a notification button.
type Action struct {
	ID    string
	Label string
}

// Notification is a desktop notification request.
type Notification struct {
	Title   string
	Body    string
	Actions []Action
}

// Notifier posts notifications.
type Notifier interface {
	Post(n Notification) error
}

// LogNotifier implements Notifier via the standard logger (M1 default).
type LogNotifier struct {
	Log *log.Logger
}

// Post logs the notification. Bodies must already be redacted by callers.
func (l LogNotifier) Post(n Notification) error {
	l.Log.Printf("notify: %s — %s (actions=%d)", n.Title, n.Body, len(n.Actions))
	return nil
}
