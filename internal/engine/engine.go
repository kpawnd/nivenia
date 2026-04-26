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
	"nivenia/internal/nivlog"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

// maxConsecutiveRestoreFailures: after this many failed restores in a
// row, the daemon transitions to ModeThawed and stops trying. Without
// this gate, a deleted snapshot or a persistent rsync failure caused
// every boot to dirty state forever.
const maxConsecutiveRestoreFailures = 3

var errRestoreAlreadyRunning = errors.New("restore already running")

const defaultLockPath = "/var/lib/nivenia/restore.lock"

// Engine runs boot restores. RestoreFn, VerifyFn, LockFile, and Log are
// all optional; production code constructs an Engine via New() and
// gets all defaults. Tests inject mocks for everything.
type Engine struct {
	Policy    config.Policy
	RestoreFn func(ctx context.Context, log *nivlog.Logger, managedRoot string, restorePaths []string) error
	VerifyFn  func(managedRoot string) error
	LockFile  string
	Log       *nivlog.Logger
}

func New(p config.Policy) Engine {
	return Engine{Policy: p}
}

func (e *Engine) lockPath() string {
	if e.LockFile != "" {
		return e.LockFile
	}
	return defaultLockPath
}

func (e *Engine) restoreBaseline(ctx context.Context) error {
	if e.RestoreFn != nil {
		return e.RestoreFn(ctx, e.Log, e.Policy.ManagedRoot, e.Policy.RestorePaths)
	}
	return restore.RestoreFromBaseline(ctx, e.Log, e.Policy.ManagedRoot, e.Policy.RestorePaths)
}

func (e *Engine) verifySnapshot() error {
	if e.VerifyFn != nil {
		return e.VerifyFn(e.Policy.ManagedRoot)
	}
	return integrity.VerifySnapshotOnly(e.Policy.ManagedRoot)
}

// ensureLog returns e.Log if set, otherwise creates a Logger writing
// to the policy's LogFile. We lazy-init so callers (tests, recovery
// flows) don't have to pass a logger explicitly when they don't have
// one — but production code always sets Log via main() so the
// component/version fields are populated.
func (e *Engine) ensureLog() *nivlog.Logger {
	if e.Log == nil {
		// Detail dir is optional; nivlog writes detail files only if
		// the dir can be created. In test temp dirs this may differ
		// from production /var/log/nivenia.
		detailDir := ""
		if e.Policy.LogFile != "" {
			detailDir = filepath.Dir(e.Policy.LogFile)
		}
		e.Log = nivlog.New(e.Policy.LogFile, detailDir, "engine", "", "")
	}
	return e.Log
}

func (e *Engine) info(event string, kv ...any)       { e.ensureLog().Info(event, kv...) }
func (e *Engine) warn(event string, kv ...any)       { e.ensureLog().Warn(event, kv...) }
func (e *Engine) errorEvent(event string, kv ...any) { e.ensureLog().Error(event, kv...) }

// lockLooksActive returns true if the lock file belongs to a living
// process. It reads the PID written by acquireRestoreLock and sends
// signal 0; if the process is gone (e.g. after a reboot or crash), the
// lock is considered stale.
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
// failures so the daemon stops dirtying state on every boot. The lab
// is effectively unprotected until an admin runs `niveniactl freeze`,
// but that is strictly preferable to an infinite loop of failed
// restores.
func (e *Engine) autoThaw(s state.State, reason string) state.State {
	s.Mode = state.ModeThawed
	s.LastRestoreOK = true
	s.FailureCount = 0
	s.LastMessage = fmt.Sprintf("auto-thawed after %d consecutive failures (%s); run 'sudo niveniactl freeze' to recapture baseline", maxConsecutiveRestoreFailures, reason)
	return s
}

// RunBootRestore is the daemon's main entry point. It loads state,
// applies mode-specific shortcuts, verifies the snapshot, runs the
// rsync, and updates state at every step.
//
// Pointer receiver because the engine lazily initialises Log on first
// use; we want the same logger (and its session ID) for every call
// within one RunBootRestore invocation.
func (e *Engine) RunBootRestore(ctx context.Context) error {
	e.info("boot.start",
		"managed_root", e.Policy.ManagedRoot,
		"restore_paths", strings.Join(e.Policy.RestorePaths, ","),
		"state_file", e.Policy.StateFile,
	)

	s, err := state.Load(e.Policy.StateFile)
	if err != nil {
		e.errorEvent("state.load.fail", "error", err.Error())
		return err
	}
	e.info("state.loaded",
		"mode", string(s.Mode),
		"last_ok", s.LastRestoreOK,
		"failure_count", s.FailureCount,
	)

	// Auto-thaw threshold: >= N consecutive failures means we stop
	// trying. With maxConsecutiveRestoreFailures = 3, the boots run
	// like this:
	//
	//   boot 1: rsync fails, failure_count=1, message "(1/3)"
	//   boot 2: rsync fails, failure_count=2, message "(2/3)"
	//   boot 3: rsync fails, failure_count=3, message "(3/3)"
	//   boot 4: this branch fires (3 >= 3) -> auto-thaw, no rsync
	//
	// Using `>=` makes the user-visible counter ("(N/3)") match the
	// threshold exactly: the third failed boot says "(3/3)" and the
	// next boot auto-thaws.
	//
	// We do this BEFORE the lock so even a stuck-lock scenario can't
	// keep us in failure mode.
	if s.Mode == state.ModeFrozen && s.FailureCount >= maxConsecutiveRestoreFailures {
		s = e.autoThaw(s, "see /var/log/nivenia.log for details")
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			e.warn("state.save.fail", "phase", "auto_thaw", "error", err.Error())
		}
		e.info("auto_thaw", "message", s.LastMessage)
		return nil
	}

	lockPath := e.lockPath()
	_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
	err = acquireRestoreLock(lockPath)
	if err != nil {
		if !errors.Is(err, errRestoreAlreadyRunning) {
			e.errorEvent("lock.acquire.fail", "error", err.Error())
			return err
		}
		s.LastRestoreOK = false
		s.LastMessage = "restore skipped: another restore process is active"
		_ = state.Save(e.Policy.StateFile, s)
		e.warn("lock.busy", "message", s.LastMessage)
		return nil
	}
	defer os.Remove(lockPath)

	switch s.Mode {
	case state.ModeThawed:
		s.LastRestoreOK = true
		s.FailureCount = 0
		s.LastMessage = "thawed mode: restore skipped"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			e.warn("state.save.fail", "phase", "thawed_skip", "error", err.Error())
		}
		e.info("mode.thawed.skip")
		return nil
	case state.ModeThawOnce:
		s.Mode = state.ModeFrozen
		s.LastRestoreOK = true
		s.FailureCount = 0
		s.LastMessage = "thaw_once consumed: restore skipped this boot"
		if err := state.Save(e.Policy.StateFile, s); err != nil {
			e.warn("state.save.fail", "phase", "thaw_once_consumed", "error", err.Error())
		}
		e.info("mode.thaw_once.consumed")
		return nil
	}

	// Snapshot verification — fail closed if the snapshot's identity
	// doesn't match what we recorded at freeze time. rsync --delete
	// against the wrong snapshot would wipe the wrong files.
	if err := e.verifySnapshot(); err != nil {
		s.FailureCount++
		s.LastRestoreOK = false
		s.LastMessage = fmt.Sprintf("snapshot verification failed (%d/%d): %v", s.FailureCount, maxConsecutiveRestoreFailures, err)
		_ = state.Save(e.Policy.StateFile, s)
		e.errorEvent("verify.fail",
			"failure_count", s.FailureCount,
			"max", maxConsecutiveRestoreFailures,
			"error", err.Error(),
		)
		return err
	}
	e.info("verify.ok")

	if err := e.restoreBaseline(ctx); err != nil {
		s.FailureCount++
		s.LastRestoreOK = false
		s.LastMessage = fmt.Sprintf("restore failed (%d/%d): %v", s.FailureCount, maxConsecutiveRestoreFailures, err)
		_ = state.Save(e.Policy.StateFile, s)
		e.errorEvent("restore.fail",
			"failure_count", s.FailureCount,
			"max", maxConsecutiveRestoreFailures,
			"error", err.Error(),
		)
		return err
	}

	s.FailureCount = 0
	s.LastRestoreOK = true
	s.LastMessage = "restore completed"
	if err := state.Save(e.Policy.StateFile, s); err != nil {
		e.warn("state.save.fail", "phase", "restore_complete", "error", err.Error())
		return err
	}
	e.info("boot.done")
	return nil
}
