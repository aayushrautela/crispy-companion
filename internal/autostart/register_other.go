//go:build !linux && !darwin && !windows

package autostart

import (
	"errors"
	"log"
	"runtime"
)

// register has no mechanism to write on other platforms; the daemon
// logs the failure and retries at the next start.
func register(exePath string, logger *log.Logger) (string, error) {
	return "", errors.New("autostart registration is not supported on " + runtime.GOOS)
}
