//go:build !windows

package engine

import (
	"errors"
	"syscall"
)

// pidIsAlive returns true if the process with the given PID exists.
func pidIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	// nil  → process alive
	// EPERM → process alive but we lack permission to signal it
	// ESRCH → process does not exist
	return err == nil || errors.Is(err, syscall.EPERM)
}
