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
	"nivenia/internal/integrity"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

const maxConsecutiveRestoreFailures = 3

var errRestoreAlreadyRunning = errors.New("restore already running")

const defaultLockPath = "/var/lib/nivenia/restore.lock"

// Engine runs boot restores. RestoreFn, VerifyFn and LockFile are optional
// overrides used in tests; production code leaves them nil/empty and gets
// the defaults (restore.RestoreFromBaseline, integrity.VerifySnapshotOnly,
// and /var/lib/nivenia/restore.lock respectively).
type Engine struct {
	Policy    config.Policy
	RestoreFn func(ctx context.Context, managedRoot string, restorePaths []string) error
	VerifyFn  func(managedRoot string) error
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

func (e Engine) verifySnapshot() error {
	if e.VerifyFn != nil {
		return e.VerifyFn(e.Policy.ManagedRoot)
	}
	return integrity.VerifySnapshotOnly(e.Policy.ManagedRoot)
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

// autoThaw transitions the engine into thawed mode after persistent
// failures so the daemon stops dirtying state on every boot. The lab is
// effectively unprotected until an admin runs `niveniactl freeze`, but
// that is strictly preferable to an infinite loop of failed restores
// (the prior behaviour, where maxConsecutiveRestoreFailures was only
// rendered into log strings and never actually enforced).
//
// The trigger is FailureCount > max so that the user-visible counter
// reaches the documented limit (e.g. "restore failed (3/3)") on the run
// that hits it, and the next run is the one that flips to thawed. This
// gives an admin a clear "we tried 3 times and gave up" trail in the
// log file, with the auto-thaw decision happening on a separate boot.
func (e Engine) autoThaw(s state.State, reason string) state.State {
	s.Mode = state.ModeThawed
	s.LastRestoreOK = true
	s.FailureCount = 0
	s.LastMessage = fmt.Sprintf("auto-thawed after %d consecutive failures (%s); run 'sudo niveniactl freeze' to recapture baseline", maxConsecutiveRestoreFailures, reason)
	return s
}

func (e Engine) RunBootRestore(ctx context.Context) error {
	s, err := state.Load(e.Policy.StateFile)
	if err != nil {
		return err
	}

	// If the previous boots have failed more than the threshold, switch to
	// thawed mode so we stop trying. We do this BEFORE acquiring the lock —
	// no point in serialising auto-thaw with anything else, and we want the
	// fast path even if the lock file is unreachable.
	if s.Mode == state.ModeFrozen && s.FailureCount > maxConsecutiveRestoreFailures {
		s = e.autoThaw(s, "see /var/log/nivenia.log for details")
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			appendLog(e.Policy.LogFile, "warn: could not save state during auto-thaw: "+err.Error())
		}
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
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
		s.FailureCount = 0
		s.LastMessage = "thawed mode: restore skipped"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			appendLog(e.Policy.LogFile, "warn: could not save state: "+err.Error())
		}
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	case state.ModeThawOnce:
		s.Mode = state.ModeFrozen
		s.LastRestoreOK = true
		s.FailureCount = 0
		s.LastMessage = "thaw_once consumed: restore skipped this boot"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			appendLog(e.Policy.LogFile, "warn: could not save state after thaw_once: "+err.Error())
		}
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	}

	// Fail-closed snapshot verification: if the APFS snapshot we're about to
	// rsync from has been swapped, has a different XID than at freeze time,
	// or lives on a different volume UUID, we refuse to run rsync. Because
	// restore uses --delete, operating against the wrong snapshot would
	// delete live files to match an unintended source. A failed verify is
	// treated like any other restore failure so the failure counter and
	// state message reflect it.
	if err := e.verifySnapshot(); err != nil {
		s.FailureCount++
		s.LastRestoreOK = false
		s.LastMessage = fmt.Sprintf("snapshot verification failed (%d/%d): %v", s.FailureCount, maxConsecutiveRestoreFailures, err)
		_ = state.Save(e.Policy.StateFile, s)
		appendLog(e.Policy.LogFile, s.LastMessage)
		return err
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
