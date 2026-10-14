package notify

import (
	"github.com/crispy-streaming/crispy-companion/internal/portal"
)

// Portal notification ID for pairing confirmations. Reusing the ID updates
// the notification in place instead of stacking (spec: same ID = update).
const pairingNotificationID = "crispy-pairing"

// PortalNotifier implements Notifier via the XDG Notification portal.
// Action buttons use non-"app." names so clicks arrive back over our
// ActionInvoked subscription instead of D-Bus activation.
type PortalNotifier struct {
	Conn *portal.Conn
	// OnAction handles (notificationID, action) activations, e.g. Revoke.
	OnAction func(notificationID, action string)
}

// NewPortal wires a portal connection to the notifier contract.
func NewPortal(c *portal.Conn, onAction func(notificationID, action string)) *PortalNotifier {
	p := &PortalNotifier{Conn: c, OnAction: onAction}
	_ = c.OnAction(func(id, action string) {
		if p.OnAction != nil {
			p.OnAction(id, action)
		}
	})
	return p
}

// Post sends the notification; actions become portal buttons.
func (p PortalNotifier) Post(n Notification) error {
	btns := make([]portal.Button, 0, len(n.Actions))
	for _, a := range n.Actions {
		if a.ID == "" || a.Label == "" {
			continue
		}
		if len(a.ID) > 7 && a.ID[:4] == "app." {
			continue // would route to D-Bus activation; never valid here
		}
		btns = append(btns, portal.Button{Label: a.Label, Action: a.ID})
	}
	return p.Conn.Notify(pairingNotificationID, n.Title, n.Body, btns)
}
