package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Mode string

const (
	ModeFrozen   Mode = "frozen"
	ModeThawed   Mode = "thawed"
	ModeThawOnce Mode = "thaw_once"
)

type State struct {
	Mode          Mode   `json:"mode"`
	LastRestoreOK bool   `json:"last_restore_ok"`
	LastMessage   string `json:"last_message"`
	UpdatedAtUTC  string `json:"updated_at_utc"`
}

func Default() State {
	return State{Mode: ModeFrozen, LastRestoreOK: true, LastMessage: "initialized", UpdatedAtUTC: time.Now().UTC().Format(time.RFC3339)}
}

func Load(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return State{}, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("parse state: %w", err)
	}
	if s.Mode == "" {
		s.Mode = ModeFrozen
	}
	return s, nil
}

func Save(path string, s State) error {
	s.UpdatedAtUTC = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0o644); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp state: %w", err)
	}
	return nil
}
