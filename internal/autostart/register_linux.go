//go:build linux

package autostart

import (
	"log"
	"os"
	"path/filepath"
)

// register writes the XDG autostart entry at user scope. main.go prefers
// the desktop Background portal whenever one is available; this direct
// write is the fallback for hosts without a portal (minimal sessions,
// headless boxes). A user-scope entry with the same file name shadows
// the system one a distro package installed, so the two never double.
//
// main.go never calls it inside a Flatpak (see autostart.InFlatpak):
// there it would write into the sandbox's private home, which no desktop
// session reads.
func register(exePath string, logger *log.Logger) (string, error) {
	path, err := xdgEntryPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(XDGEntry(exePath)), 0o644); err != nil {
		return "", err
	}
	logger.Printf("autostart: wrote %s", path)
	return MethodXDG, nil
}

// xdgEntryPath follows the XDG base directory spec:
// $XDG_CONFIG_HOME/autostart, defaulting to ~/.config/autostart.
func xdgEntryPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", EntryName+".desktop"), nil
}
