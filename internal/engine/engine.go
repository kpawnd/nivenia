package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

const maxConsecutiveRestoreFailures = 3

var errRestoreAlreadyRunning = errors.New("restore already running")

const restoreLockStaleAfter = 30 * time.Minute

type Engine struct {
	Policy config.Policy
}

func New(p config.Policy) Engine {
	return Engine{Policy: p}
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

func writeLockMetadata(path string) error {
	meta := time.Now().UTC().Format(time.RFC3339) + "\n"
	return os.WriteFile(path, []byte(meta), 0o644)
}

func lockLooksActive(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) < restoreLockStaleAfter
}

func acquireRestoreLock(path string) error {
	for attempts := 0; attempts < 2; attempts++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			if err := writeLockMetadata(path); err != nil {
				_ = os.Remove(path)
				return err
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

func (e Engine) RunBootRestore() error {
	s, err := state.Load(e.Policy.StateFile)
	if err != nil {
		return err
	}

	lockPath := "/var/lib/nivenia/restore.lock"
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

	marker := "/var/lib/nivenia/restore.started"
	_ = os.MkdirAll(filepath.Dir(marker), 0o755)

	switch s.Mode {
	case state.ModeThawed:
		s.LastRestoreOK = true
		s.LastMessage = "thawed mode: restore skipped"
		_ = state.Save(e.Policy.StateFile, s)
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	case state.ModeThawOnce:
		s.Mode = state.ModeFrozen
		s.LastRestoreOK = true
		s.LastMessage = "thaw_once consumed: restore skipped this boot"
		_ = state.Save(e.Policy.StateFile, s)
		appendLog(e.Policy.LogFile, s.LastMessage)
		return nil
	}

	_ = os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)

	if err := restore.RestoreFromBaseline(e.Policy.BaselineRoot, e.Policy.ManagedRoot, e.Policy.ExcludePaths); err != nil {
		s.FailureCount++
		s.LastRestoreOK = false
		s.LastMessage = fmt.Sprintf("restore failed (%d/%d): %v", s.FailureCount, maxConsecutiveRestoreFailures, err)
		if s.FailureCount >= maxConsecutiveRestoreFailures {
			s.LastMessage = fmt.Sprintf("restore failed %d times; frozen mode preserved (no auto-thaw): %v", s.FailureCount, err)
		}
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
	_ = os.Remove(marker)
	appendLog(e.Policy.LogFile, s.LastMessage)
	return nil
}
