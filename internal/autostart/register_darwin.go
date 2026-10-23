//go:build darwin

package autostart

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// register writes the LaunchAgent plist and asks launchd to load it. A
// launchctl failure never fails the registration: the plist is on disk,
// so the agent starts at the next login regardless — and "already
// loaded" (a re-registration, or a launchd we cannot talk to) lands in
// that path too.
func register(exePath string, logger *log.Logger) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", EntryName+".plist")
	logDir := filepath.Join(home, "Library", "Logs", "Crispy Companion")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(plistPath, []byte(LaunchAgentPlist(exePath, logDir)), 0o644); err != nil {
		return "", err
	}
	logger.Printf("autostart: wrote %s", plistPath)

	// Modern launchd, then the legacy spelling. Both are best effort.
	target := fmt.Sprintf("gui/%d", os.Getuid())
	if err := launchctl(logger, "bootstrap", target, plistPath); err != nil {
		_ = launchctl(logger, "load", "-w", plistPath)
	}
	return MethodLaunchAgent, nil
}

func launchctl(logger *log.Logger, args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		logger.Printf("autostart: launchctl %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return err
}
