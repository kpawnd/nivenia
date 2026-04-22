package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

const maxConsecutiveRestoreFailures = 3

var errRestoreAlreadyRunning = errors.New("restore already running")

const defaultLockPath = "/var/lib/nivenia/restore.lock"

// Engine runs boot restores. RestoreFn and LockFile are optional overrides
// used in tests; production code leaves them nil/empty and gets the defaults.
type Engine struct {
	Policy    config.Policy
	RestoreFn func(ctx context.Context, managedRoot string, restorePaths []string) error
	LockFile  string
}

func New(p config.Policy) Engine {
	return Engine{Policy: p}
}

func (e Engine) lockPath() string {
	if e.LockFile != "" {
		return e.LockFile
	}
	return defaultLockPath
}

func (e Engine) restoreBaseline(ctx context.Context) error {
	if e.RestoreFn != nil {
		return e.RestoreFn(ctx, e.Policy.ManagedRoot, e.Policy.RestorePaths)
	}
	return restore.RestoreFromBaseline(ctx, e.Policy.ManagedRoot, e.Policy.RestorePaths)
}

func appendLog(path, msg string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().UTC().Format(time.RFC3339) + " " + msg + "\n")
}

// lockLooksActive returns true if the lock file belongs to a living process.
// It reads the PID written by acquireRestoreLock and sends signal 0; if the
// process is gone (e.g. after a reboot or crash) the lock is considered stale.
func lockLooksActive(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) >= 1 {
		if pid, parseErr := strconv.Atoi(strings.TrimSpace(lines[0])); parseErr == nil && pid > 0 {
			return pidIsAlive(pid)
		}
	}
	// Legacy lock file without PID: fall back to a short mtime window.
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < 5*time.Minute
}

func acquireRestoreLock(path string) error {
	for attempts := 0; attempts < 2; attempts++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			// Write PID before closing so there is never a window where the lock
			// file exists but is empty (which would look stale to lockLooksActive).
			meta := fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			_, werr := f.WriteString(meta)
			_ = f.Close()
			if werr != nil {
				_ = os.Remove(path)
				return werr
			}
			return nil
		}
		if !os.IsExist(err) {
			return err
		}

		if lockLooksActive(path) {
			return errRestoreAlreadyRunning
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return errRestoreAlreadyRunning
		}
	}

	return errRestoreAlreadyRunning
}

func (e Engine) RunBootRestore(ctx context.Context) error {
	s, err := state.Load(e.Policy.StateFile)
	if err != nil {
		return err
	}

	lockPath := e.lockPath()
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
	err = acquireRestoreLock(lockPath)
	if err != nil {
		if !errors.Is(err, errRestoreAlreadyRunning) {
			return err
		}
		s.LastRestoreOK = false
		s.LastMessage = "restore skipped: another restore process is active"
		_ = state.Save(e.Policy.StateFile, s)
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	}
	defer os.Remove(lockPath)

	switch s.Mode {
	case state.ModeThawed:
		s.LastRestoreOK = true
		s.LastMessage = "thawed mode: restore skipped"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			appendLog(e.Policy.LogFile, "warn: could not save state: "+err.Error())
		}
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	case state.ModeThawOnce:
		s.Mode = state.ModeFrozen
		s.LastRestoreOK = true
		s.LastMessage = "thaw_once consumed: restore skipped this boot"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			appendLog(e.Policy.LogFile, "warn: could not save state after thaw_once: "+err.Error())
		}
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	}

	appendLog(e.Policy.LogFile, "restore started")

	if err := e.restoreBaseline(ctx); err != nil {
		s.FailureCount++
		s.LastRestoreOK = false
		s.LastMessage = fmt.Sprintf("restore failed (%d/%d): %v", s.FailureCount, maxConsecutiveRestoreFailures, err)
		_ = state.Save(e.Policy.StateFile, s)
		appendLog(e.Policy.LogFile, s.LastMessage)
		return err
	}

	s.FailureCount = 0
	s.LastRestoreOK = true
	s.LastMessage = "restore completed"
	if err := state.Save(e.Policy.StateFile, s); err != nil {
		return err
	}
	appendLog(e.Policy.LogFile, s.LastMessage)
	return nil
}
