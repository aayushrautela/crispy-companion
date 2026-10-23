package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A bare engine name resolves next to the companion binary when PATH has
// nothing: the OS autostart launch (launchd CWD "/", Windows Run CWD
// system32) has no useful PATH, but both packaged installs ship the
// engine beside the companion.
func TestResolveBinaryFindsEngineNextToCompanion(t *testing.T) {
	dir := t.TempDir()
	enginePath := filepath.Join(dir, "torrserver-no-such-binary-xyz")
	if err := os.WriteFile(enginePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveBinary("torrserver-no-such-binary-xyz", dir)
	if got != enginePath {
		t.Fatalf("resolve: got %q want %q", got, enginePath)
	}
}

// An explicit path is never second-guessed.
func TestResolveBinaryKeepsExplicitPath(t *testing.T) {
	p := filepath.Join("/opt", "somewhere", "torrserver")
	if got := resolveBinary(p, "/tmp"); got != p {
		t.Fatalf("resolve: got %q want %q", got, p)
	}
}

// Nothing found anywhere: the configured name comes back untouched and the
// caller fails exactly as before.
func TestResolveBinaryUnknownBareNameStays(t *testing.T) {
	got := resolveBinary("torrserver-no-such-binary-xyz", t.TempDir())
	if got != "torrserver-no-such-binary-xyz" {
		t.Fatalf("resolve: got %q", got)
	}
}

// A directory of that name is not a binary.
func TestResolveBinaryIgnoresDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "torrserver-no-such-binary-xyz"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveBinary("torrserver-no-such-binary-xyz", dir)
	if got != "torrserver-no-such-binary-xyz" {
		t.Fatalf("resolve: got %q", got)
	}
}
