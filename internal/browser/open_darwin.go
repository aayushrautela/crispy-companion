//go:build darwin

package browser

import "os/exec"

// open uses macOS's own opener.
func open(u string) error {
	return exec.Command("open", u).Run()
}
