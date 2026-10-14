package pairing_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crispy-streaming/crispy-companion/internal/pairing"
)

func TestMintLookupRevoke(t *testing.T) {
	dir := t.TempDir()
	s, err := pairing.Open(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Mint("u1", "alice@example.com", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 32 {
		t.Fatalf("token too short: %q", tok)
	}
	got, ok := s.Lookup(tok)
	if !ok || got.Email != "alice@example.com" {
		t.Fatalf("lookup failed: %+v %v", got, ok)
	}
	if _, ok := s.Lookup(tok + "x"); ok {
		t.Fatal("forged token accepted")
	}
	if err := s.Revoke(tok); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(tok); ok {
		t.Fatal("revoked token still valid")
	}
	if s.Count() != 0 {
		t.Fatalf("count = %d, want 0", s.Count())
	}
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.json")
	s, _ := pairing.Open(p)
	tok, err := s.Mint("u1", "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := pairing.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Lookup(tok); !ok {
		t.Fatal("token did not survive reopen")
	}
}

func TestWindowSinglePendingAndRateLimit(t *testing.T) {
	w := pairing.NewWindow(90*time.Second, 2)
	id, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Begin(); !errors.Is(err, pairing.ErrPendingExists) {
		t.Fatalf("second begin: got %v, want ErrPendingExists", err)
	}
	w.Clear(id)
	if _, err := w.Begin(); err != nil {
		t.Fatalf("begin after clear: %v", err)
	}
	pending, _, _ := w.State()
	if !pending {
		t.Fatal("expected pending state")
	}
}

func TestWindowRateLimit(t *testing.T) {
	w := pairing.NewWindow(time.Second, 1)
	id, err := w.Begin()
	if err != nil {
		t.Fatal(err)
	}
	w.Clear(id)
	if _, err := w.Begin(); !errors.Is(err, pairing.ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", err)
	}
}
