package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"nivenia/internal/config"
	"nivenia/internal/state"
)

// newTestEngine creates an Engine pointing entirely at a temp directory so
// tests never touch /var/lib or /var/log.
func newTestEngine(
	t *testing.T,
	mode state.Mode,
	restoreFn func(ctx context.Context, managedRoot string, restorePaths []string) error,
) Engine {
	t.Helper()
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	logFile := filepath.Join(dir, "nivenia.log")
	lockFile := filepath.Join(dir, "restore.lock")

	s := state.State{Mode: mode, LastRestoreOK: true}
	if err := state.Save(stateFile, s); err != nil {
		t.Fatal(err)
	}

	return Engine{
		Policy: config.Policy{
			ManagedRoot:  dir,
			RestorePaths: []string{filepath.Join(dir, "Users")},
			StateFile:    stateFile,
			LogFile:      logFile,
		},
		RestoreFn: restoreFn,
		// Tests don't have a real integrity record or APFS volume; bypass
		// the snapshot verification that production engines run before
		// rsync. The default (integrity.VerifySnapshotOnly) would hit
		// /var/lib/nivenia/integrity.json and diskutil, neither of which
		// is appropriate inside unit tests.
		VerifyFn: func(string) error { return nil },
		LockFile: lockFile,
	}
}

func loadState(t *testing.T, e Engine) state.State {
	t.Helper()
	s, err := state.Load(e.Policy.StateFile)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return s
}

// ── Mode logic ────────────────────────────────────────────────────────────────

func TestRunBootRestore_Frozen_CallsRestore(t *testing.T) {
	called := false
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		called = true
		return nil
	})
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("RunBootRestore: %v", err)
	}
	if !called {
		t.Error("restore function was not called in frozen mode")
	}
	s := loadState(t, e)
	if !s.LastRestoreOK {
		t.Errorf("LastRestoreOK: got false, want true")
	}
	if s.LastMessage != "restore completed" {
		t.Errorf("LastMessage: got %q", s.LastMessage)
	}
	if s.FailureCount != 0 {
		t.Errorf("FailureCount: got %d, want 0", s.FailureCount)
	}
}

func TestRunBootRestore_Frozen_VerifyFailsAbortsBeforeRestore(t *testing.T) {
	// If snapshot verification fails we must NOT invoke rsync, because
	// rsync --delete against a mis-identified snapshot would delete live
	// user files. The restore function should be untouched and state
	// should reflect the verification failure.
	verifyErr := errors.New("snapshot XID mismatch")
	restoreCalled := false
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		restoreCalled = true
		return nil
	})
	e.VerifyFn = func(string) error { return verifyErr }

	err := e.RunBootRestore(context.Background())
	if err == nil {
		t.Fatal("expected error when verification fails")
	}
	if restoreCalled {
		t.Fatal("restore function must not be called when verification fails")
	}
	s := loadState(t, e)
	if s.LastRestoreOK {
		t.Error("LastRestoreOK should be false when verification fails")
	}
	if s.FailureCount != 1 {
		t.Errorf("FailureCount: got %d, want 1", s.FailureCount)
	}
	if !strings.Contains(s.LastMessage, "snapshot verification failed") {
		t.Errorf("LastMessage should mention verification failure: got %q", s.LastMessage)
	}
}

func TestRunBootRestore_Frozen_RestoreFails(t *testing.T) {
	restoreErr := errors.New("rsync died")
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		return restoreErr
	})
	err := e.RunBootRestore(context.Background())
	if err == nil {
		t.Fatal("expected error from failed restore")
	}
	s := loadState(t, e)
	if s.LastRestoreOK {
		t.Error("LastRestoreOK should be false after failure")
	}
	if s.FailureCount != 1 {
		t.Errorf("FailureCount: got %d, want 1", s.FailureCount)
	}
}

func TestRunBootRestore_Frozen_FailureCountAccumulates(t *testing.T) {
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		return errors.New("fail")
	})
	for i := 1; i <= 3; i++ {
		_ = e.RunBootRestore(context.Background())
		s := loadState(t, e)
		if s.FailureCount != i {
			t.Errorf("after %d failures: FailureCount = %d, want %d", i, s.FailureCount, i)
		}
		if s.Mode != state.ModeFrozen {
			t.Errorf("after %d failures: mode should still be frozen (auto-thaw fires only AFTER threshold is exceeded), got %q", i, s.Mode)
		}
	}
}

// After max consecutive failures, the daemon must stop trying — otherwise
// every boot keeps churning rsync against a dead snapshot and writing
// failure messages forever (the bug that motivated this test). The next
// boot after exceeding the threshold should auto-thaw and return success
// so launchd doesn't keep retrying.
func TestRunBootRestore_AutoThawsAfterThreshold(t *testing.T) {
	callCount := 0
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		callCount++
		return errors.New("snapshot disappeared")
	})

	// Drive failure count up to and past the threshold.
	for i := 0; i < maxConsecutiveRestoreFailures+1; i++ {
		_ = e.RunBootRestore(context.Background())
	}
	s := loadState(t, e)
	if s.FailureCount != maxConsecutiveRestoreFailures+1 {
		t.Fatalf("setup: expected FailureCount=%d, got %d", maxConsecutiveRestoreFailures+1, s.FailureCount)
	}
	if s.Mode != state.ModeFrozen {
		t.Fatalf("setup: still expected frozen before auto-thaw boot, got %q", s.Mode)
	}

	priorRestoreCalls := callCount

	// Next boot: must auto-thaw, NOT call restore, and return nil.
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("auto-thaw boot must succeed (returned %v)", err)
	}
	if callCount != priorRestoreCalls {
		t.Errorf("restore function must not run during auto-thaw boot (calls: %d -> %d)", priorRestoreCalls, callCount)
	}
	s = loadState(t, e)
	if s.Mode != state.ModeThawed {
		t.Errorf("after auto-thaw: mode = %q, want thawed", s.Mode)
	}
	if !s.LastRestoreOK {
		t.Error("after auto-thaw: LastRestoreOK should be true so launchd considers the boot healthy")
	}
	if s.FailureCount != 0 {
		t.Errorf("after auto-thaw: FailureCount should reset to 0, got %d", s.FailureCount)
	}
	if !strings.Contains(s.LastMessage, "auto-thawed") || !strings.Contains(s.LastMessage, "niveniactl freeze") {
		t.Errorf("after auto-thaw: LastMessage should mention auto-thaw and recovery command, got %q", s.LastMessage)
	}

	// Subsequent boots in thawed mode must also skip restore.
	priorRestoreCalls = callCount
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("post-auto-thaw boot must succeed: %v", err)
	}
	if callCount != priorRestoreCalls {
		t.Errorf("restore function must not run in thawed mode (calls: %d -> %d)", priorRestoreCalls, callCount)
	}
}

func TestRunBootRestore_Thawed_SkipsRestore(t *testing.T) {
	called := false
	e := newTestEngine(t, state.ModeThawed, func(_ context.Context, _ string, _ []string) error {
		called = true
		return nil
	})
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("RunBootRestore: %v", err)
	}
	if called {
		t.Error("restore function should not be called in thawed mode")
	}
	s := loadState(t, e)
	if s.Mode != state.ModeThawed {
		t.Errorf("mode should remain thawed, got %q", s.Mode)
	}
}

func TestRunBootRestore_ThawOnce_SkipsAndSetsFrozen(t *testing.T) {
	called := false
	e := newTestEngine(t, state.ModeThawOnce, func(_ context.Context, _ string, _ []string) error {
		called = true
		return nil
	})
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("RunBootRestore: %v", err)
	}
	if called {
		t.Error("restore function should not be called in thaw_once mode")
	}
	s := loadState(t, e)
	if s.Mode != state.ModeFrozen {
		t.Errorf("mode should become frozen after thaw_once, got %q", s.Mode)
	}
	if !s.LastRestoreOK {
		t.Error("LastRestoreOK should be true after thaw_once skip")
	}
}

// ── Log ───────────────────────────────────────────────────────────────────────

func TestRunBootRestore_LogsEvents(t *testing.T) {
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		return nil
	})
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.Policy.LogFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if !strings.Contains(log, "restore started") {
		t.Error("log should contain 'restore started'")
	}
	if !strings.Contains(log, "restore completed") {
		t.Error("log should contain 'restore completed'")
	}
}

// ── Lock ──────────────────────────────────────────────────────────────────────

func TestRunBootRestore_LockRemovedAfterSuccess(t *testing.T) {
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		return nil
	})
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.LockFile); !os.IsNotExist(err) {
		t.Error("lock file should be removed after restore completes")
	}
}

func TestRunBootRestore_LockRemovedAfterFailure(t *testing.T) {
	e := newTestEngine(t, state.ModeFrozen, func(_ context.Context, _ string, _ []string) error {
		return errors.New("fail")
	})
	_ = e.RunBootRestore(context.Background())
	if _, err := os.Stat(e.LockFile); !os.IsNotExist(err) {
		t.Error("lock file should be removed even after restore failure")
	}
}

func TestRunBootRestore_StaleLockFromDeadPID_IsCleared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PID-based lock not supported on Windows")
	}
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "restore.lock")

	// Write a lock file with a PID that is guaranteed not to exist (> 4M).
	deadPID := 4194304
	content := strconv.Itoa(deadPID) + "\n2026-04-22T00:00:00Z\n"
	if err := os.WriteFile(lockFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	stateFile := filepath.Join(dir, "state.json")
	logFile := filepath.Join(dir, "nivenia.log")
	if err := state.Save(stateFile, state.State{Mode: state.ModeFrozen}); err != nil {
		t.Fatal(err)
	}
	e := Engine{
		Policy: config.Policy{
			ManagedRoot:  dir,
			RestorePaths: []string{},
			StateFile:    stateFile,
			LogFile:      logFile,
		},
		RestoreFn: func(_ context.Context, _ string, _ []string) error {
			called = true
			return nil
		},
		VerifyFn: func(string) error { return nil },
		LockFile: lockFile,
	}
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("RunBootRestore: %v", err)
	}
	if !called {
		t.Error("restore should run after clearing stale lock from dead PID")
	}
}

func TestRunBootRestore_ActiveLock_SkipsRestore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PID-based lock not supported on Windows")
	}
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "restore.lock")

	// Write a lock file with OUR OWN PID so it looks active.
	content := strconv.Itoa(os.Getpid()) + "\n2026-04-22T00:00:00Z\n"
	if err := os.WriteFile(lockFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	called := false
	stateFile := filepath.Join(dir, "state.json")
	logFile := filepath.Join(dir, "nivenia.log")
	if err := state.Save(stateFile, state.State{Mode: state.ModeFrozen}); err != nil {
		t.Fatal(err)
	}
	e := Engine{
		Policy: config.Policy{
			ManagedRoot:  dir,
			RestorePaths: []string{},
			StateFile:    stateFile,
			LogFile:      logFile,
		},
		RestoreFn: func(_ context.Context, _ string, _ []string) error {
			called = true
			return nil
		},
		VerifyFn: func(string) error { return nil },
		LockFile: lockFile,
	}
	if err := e.RunBootRestore(context.Background()); err != nil {
		t.Fatalf("RunBootRestore: %v", err)
	}
	if called {
		t.Error("restore should be skipped when lock is held by a live process")
	}
	s := loadState(t, e)
	if !strings.Contains(s.LastMessage, "another restore process is active") {
		t.Errorf("unexpected message: %q", s.LastMessage)
	}
}

// ── acquireRestoreLock unit tests ─────────────────────────────────────────────

func TestAcquireRestoreLock_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "test.lock")

	if err := acquireRestoreLock(lockFile); err != nil {
		t.Fatalf("acquireRestoreLock: %v", err)
	}
	defer os.Remove(lockFile)

	if _, err := os.Stat(lockFile); err != nil {
		t.Errorf("lock file not created: %v", err)
	}
}

func TestAcquireRestoreLock_WritesOwnPID(t *testing.T) {
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "test.lock")

	if err := acquireRestoreLock(lockFile); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockFile)

	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	if len(lines) == 0 {
		t.Fatal("lock file is empty")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		t.Fatalf("first line of lock file is not a PID: %q", lines[0])
	}
	if pid != os.Getpid() {
		t.Errorf("lock PID: got %d, want %d", pid, os.Getpid())
	}
}

func TestAcquireRestoreLock_NoEmptyWindowBetweenCreateAndWrite(t *testing.T) {
	// Lock file should never be empty after acquireRestoreLock returns nil.
	dir := t.TempDir()
	lockFile := filepath.Join(dir, "test.lock")

	if err := acquireRestoreLock(lockFile); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lockFile)

	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Error("lock file is empty immediately after acquisition — PID was not written atomically")
	}
}
