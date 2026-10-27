// Package browser opens a URL in the user's default browser.
//
// The start-menu entry uses this to answer a click with the Crispy web
// app: the daemon is headless, and the browser is the only window it can
// put in front of the user. The URL comes from configuration (a build
// default, config.toml or the environment), so it is validated here —
// an absolute http(s) URL only — before any platform opener runs; a
// misconfigured value must never turn the click into something
// unexpected.
package browser

import (
	"errors"
	"net/url"
	"strings"
)

// ErrInvalidURL reports a URL that is not an absolute http(s) URL. The
// caller distinguishes it from an opener failure: a bad URL is a
// configuration problem to log, a failed opener still deserves a
// user-facing fallback.
var ErrInvalidURL = errors.New("not an absolute http(s) URL")

// Open opens rawURL in the default browser. The openers return as soon
// as the URL is handed over; they do not follow the browser's lifetime.
//
// On Linux the opener is xdg-open — inside the Flatpak sandbox the
// runtime's own xdg-open, which forwards to the OpenURI portal (the
// org.freedesktop.portal.Desktop talk name the manifest already grants
// for notifications), and on the host the desktop's opener.
func Open(rawURL string) error {
	u, err := parse(rawURL)
	if err != nil {
		return err
	}
	return open(u.String())
}

// parse validates a configured URL: no whitespace anywhere, an absolute
// http or https scheme, and a non-empty host.
func parse(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) != rawURL || strings.ContainsAny(rawURL, " \t\v\f\r\n") {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, ErrInvalidURL
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ErrInvalidURL
	}
	return u, nil
}
