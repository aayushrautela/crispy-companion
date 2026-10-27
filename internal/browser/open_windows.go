//go:build windows

package browser

import "os/exec"

// open uses the shell's URL protocol handler — the default browser —
// without a console window or shell quoting (the URL travels as one
// argv element).
func open(u string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Run()
}
