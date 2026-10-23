package autostart

import (
	"path/filepath"
	"strings"
)

// This file builds the autostart entry contents. The builders are pure so
// every platform's entry shape is unit-tested from any OS (CI runs Linux);
// the per-OS files only perform the platform side effect.

// AutostartRunFlag tells the daemon it was started by the OS autostart
// mechanism, so it serves without registering itself again.
const AutostartRunFlag = "--autostart-run"

// XDGEntry renders the XDG autostart desktop file (Linux). It mirrors
// packaging/linux/io.crispy.Companion.autostart.desktop, which distro
// packages install system-wide; the daemon writes the same file at user
// scope when no desktop portal is there to ask.
func XDGEntry(exePath string) string {
	var b strings.Builder
	b.WriteString("[Desktop Entry]\n")
	b.WriteString("Type=Application\n")
	b.WriteString("Name=Crispy Companion\n")
	b.WriteString("Comment=Local torrent streaming engine for Crispy\n")
	b.WriteString("Exec=" + desktopExecArg(exePath) + " " + AutostartRunFlag + "\n")
	b.WriteString("Icon=" + EntryName + "\n")
	b.WriteString("Terminal=false\n")
	b.WriteString("StartupNotify=false\n")
	// Autostart-only entry: it must not show up in any menu.
	b.WriteString("NoDisplay=true\n")
	// Deferred start so the login does not wait for the companion.
	b.WriteString("X-GNOME-Autostart-Delay=90\n")
	// The daemon talks to the user through desktop notifications.
	b.WriteString("X-GNOME-UsesNotifications=true\n")
	return b.String()
}

// desktopExecArg quotes a program path for a desktop file Exec key (the
// spec's quoting is not shell quoting: only backslash, double quote,
// backtick and dollar need escaping inside double quotes).
func desktopExecArg(path string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"`", "\\`",
		`$`, `\$`,
	)
	return `"` + r.Replace(path) + `"`
}

// LaunchAgentPlist renders the macOS LaunchAgent. logDir receives the
// daemon's stdout/stderr, matching packaging/macos/io.crispy.Companion.plist.
func LaunchAgentPlist(exePath, logDir string) string {
	dir := filepath.Dir(exePath)
	// WorkingDirectory so a relative engine.binaryPath ("torrserver" in
	// the tarball next to the binary) is found the same way as in a
	// terminal launch — launchd starts agents with CWD "/".
	// KeepAlive restarts the daemon if it ever dies; RunAtLoad starts it
	// at login.
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	keyString(&b, "Label", EntryName)
	b.WriteString("    <key>ProgramArguments</key>\n    <array>\n")
	b.WriteString("        <string>" + xmlEscape(exePath) + "</string>\n")
	b.WriteString("        <string>" + xmlEscape(AutostartRunFlag) + "</string>\n")
	b.WriteString("    </array>\n")
	b.WriteString("    <key>RunAtLoad</key>\n    <true/>\n")
	b.WriteString("    <key>KeepAlive</key>\n    <true/>\n")
	keyString(&b, "WorkingDirectory", dir)
	keyString(&b, "StandardOutPath", filepath.Join(logDir, "crispy-companion.log"))
	keyString(&b, "StandardErrorPath", filepath.Join(logDir, "crispy-companion.err.log"))
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func keyString(b *strings.Builder, key, value string) {
	b.WriteString("    <key>" + key + "</key>\n    <string>" + xmlEscape(value) + "</string>\n")
}

// xmlEscape escapes the five XML specials so paths with & or < cannot
// break the plist.
func xmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace(s)
}

// RunKeyData renders the HKCU Run value data for Windows: the quoted
// program path plus the autostart flag.
func RunKeyData(exePath string) string {
	return `"` + exePath + `" ` + AutostartRunFlag
}
