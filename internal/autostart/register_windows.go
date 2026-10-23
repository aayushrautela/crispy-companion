//go:build windows

package autostart

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// runKeyFile is the per-user Run list the shell starts at sign-in.
const runKeyFile = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// runKeyValue is the display name in Task Manager's startup tab; it is
// also what the old manual instructions in packaging/windows/README.md
// told users to create, so existing installs keep upgrading in place.
const runKeyValue = "Crispy Companion"

// register writes the HKCU Run value through reg.exe, which ships with
// every Windows. Going through the tool keeps the daemon free of a
// registry binding (the repo vendors godbus only, and a cgo-free
// dependency would mean vendoring another module for one write).
func register(exePath string, logger *log.Logger) (string, error) {
	cmd := exec.Command("reg", "add", runKeyFile,
		"/v", runKeyValue,
		"/t", "REG_SZ",
		"/d", RunKeyData(exePath),
		"/f")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("reg add: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	logger.Printf("autostart: wrote %s\\%s", runKeyFile, runKeyValue)
	return MethodRegistry, nil
}
