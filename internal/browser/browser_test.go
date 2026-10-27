package browser

import (
	"errors"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := map[string]string{ // raw URL → expected host
		"https://web.crispytv.tech":          "web.crispytv.tech",
		"https://web.crispytv.tech/pair?x=1": "web.crispytv.tech",
		"http://localhost:5173":              "localhost:5173",
		"http://127.0.0.1:8080/#/settings":   "127.0.0.1:8080",
	}
	for raw, host := range cases {
		u, err := parse(raw)
		if err != nil {
			t.Fatalf("parse(%q) = %v, want ok", raw, err)
		}
		if u.Host != host {
			t.Fatalf("parse(%q).Host = %q, want %q", raw, u.Host, host)
		}
	}
}

// The URL comes from configuration, so anything that is not an absolute
// http(s) URL must be rejected — a misconfigured value must never reach
// the platform opener.
func TestParseInvalid(t *testing.T) {
	for _, raw := range []string{
		"",
		"notaurl",
		"ftp://web.crispytv.tech",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"https://",             // no host
		"https:///path",        // no host
		" https://web.example", // leading whitespace
		"https://web.example ", // trailing whitespace
		"https://web exam.pl",  // embedded whitespace
		"https://web.example\n",
	} {
		if _, err := parse(raw); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("parse(%q) = %v, want ErrInvalidURL", raw, err)
		}
	}
}

// Open validates before any platform opener runs, so tests never spawn
// a real browser.
func TestOpenValidatesFirst(t *testing.T) {
	if err := Open("javascript:alert(1)"); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("Open of an invalid URL = %v, want ErrInvalidURL", err)
	}
}
