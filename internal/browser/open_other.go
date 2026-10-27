//go:build !linux && !darwin && !windows

package browser

import (
	"errors"
	"runtime"
)

// open has no opener on other platforms; the caller degrades to its
// notification fallback.
func open(string) error {
	return errors.New("opening a browser is not supported on " + runtime.GOOS)
}
