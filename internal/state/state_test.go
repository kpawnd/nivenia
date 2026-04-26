package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	s := Default()
	if s.Mode != ModeFrozen {
		t.Errorf("Default mode: got %q, want %q", s.Mode, ModeFrozen)
	}
	if !s.LastRestoreOK {
		t.Error("Default LastRestoreOK should be true")
	}
	if s.UpdatedAtUTC == "" {
		t.Error("Default UpdatedAtUTC should not be empty")
	}
}

func TestLoad_MissingFile_ReturnsDefault(t *testing.T) {
	s, err := Load("/nonexistent/path/state.json")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if s.Mode != ModeFrozen {
		t.Errorf("missing file: got mode %q, want %q", s.Mode, ModeFrozen)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_EmptyMode_DefaultsFrozen(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := os.WriteFile(p, []byte(`{"mode":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != ModeFrozen {
		t.Errorf("empty mode: got %q, want %q", s.Mode, ModeFrozen)
	}
}

func TestSaveAndLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")

	original := State{
		Mode:          ModeThawOnce,
		LastRestoreOK: false,
		LastMessage:   "test message",
		FailureCount:  2,
	}

	if err := Save(p, original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Mode != original.Mode {
		t.Errorf("Mode: got %q, want %q", loaded.Mode, original.Mode)
	}
	if loaded.LastRestoreOK != original.LastRestoreOK {
		t.Errorf("LastRestoreOK: got %v, want %v", loaded.LastRestoreOK, original.LastRestoreOK)
	}
	if loaded.LastMessage != original.LastMessage {
		t.Errorf("LastMessage: got %q, want %q", loaded.LastMessage, original.LastMessage)
	}
	if loaded.FailureCount != original.FailureCount {
		t.Errorf("FailureCount: got %d, want %d", loaded.FailureCount, original.FailureCount)
	}
	if loaded.UpdatedAtUTC == "" {
		t.Error("Save should set UpdatedAtUTC")
	}
}

func TestSave_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "subdir", "nested", "state.json")
	if err := Save(p, Default()); err != nil {
		t.Fatalf("Save in non-existent subdirs: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("state file not created: %v", err)
	}
}

func TestSave_IsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := Save(p, Default()); err != nil {
		t.Fatal(err)
	}
	// Temp file should not remain after successful save.
	tmp := p + ".tmp"
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file %q should not exist after Save", tmp)
	}
}

func TestAllModes(t *testing.T) {
	for _, mode := range []Mode{ModeFrozen, ModeThawed, ModeThawOnce} {
		dir := t.TempDir()
		p := filepath.Join(dir, "state.json")
		s := State{Mode: mode}
		if err := Save(p, s); err != nil {
			t.Fatalf("Save(%s): %v", mode, err)
		}
		loaded, err := Load(p)
		if err != nil {
			t.Fatalf("Load(%s): %v", mode, err)
		}
		if loaded.Mode != mode {
			t.Errorf("mode %s: roundtrip got %q", mode, loaded.Mode)
		}
	}
}
