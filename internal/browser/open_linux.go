//go:build linux

package browser

import "os/exec"

// open hands the URL to the desktop's opener. Inside the Flatpak this
// is the runtime's xdg-open, which talks to the OpenURI portal; on the
// host it is the desktop's usual browser dispatch.
func open(u string) error {
	return exec.Command("xdg-open", u).Run()
}
