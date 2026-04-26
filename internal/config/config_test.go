package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeJSON(t *testing.T, dir string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/policy.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(p, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_AllDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{})
	policy, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ManagedRoot != "/System/Volumes/Data" {
		t.Errorf("ManagedRoot: got %q, want /System/Volumes/Data", policy.ManagedRoot)
	}
	if len(policy.RestorePaths) != 2 {
		t.Errorf("RestorePaths: got %d entries, want 2", len(policy.RestorePaths))
	}
	if policy.StateFile != "/var/lib/nivenia/state.json" {
		t.Errorf("StateFile: got %q, want /var/lib/nivenia/state.json", policy.StateFile)
	}
	if policy.LogFile != "/var/log/nivenia.log" {
		t.Errorf("LogFile: got %q, want /var/log/nivenia.log", policy.LogFile)
	}
}

func TestLoad_ExplicitValues(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/custom/root",
		"restore_paths": []string{"/custom/root/Users"},
		"state_file":    "/tmp/state.json",
		"log_file":      "/tmp/nivenia.log",
	})
	policy, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if policy.ManagedRoot != "/custom/root" {
		t.Errorf("ManagedRoot: got %q", policy.ManagedRoot)
	}
	if len(policy.RestorePaths) != 1 || policy.RestorePaths[0] != "/custom/root/Users" {
		t.Errorf("RestorePaths: got %v", policy.RestorePaths)
	}
	if policy.PolicyPath != p {
		t.Errorf("PolicyPath: got %q, want %q", policy.PolicyPath, p)
	}
}

func TestLoad_DefaultRestorePathsUseCustomManagedRoot(t *testing.T) {
	dir := t.TempDir()
	const root = "/vol/data"
	p := writeJSON(t, dir, map[string]any{
		"managed_root": root,
	})
	policy, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// Defaults are POSIX-joined regardless of host OS — these are
	// macOS paths and must round-trip with forward slashes intact.
	for _, rp := range policy.RestorePaths {
		if !strings.HasPrefix(rp, root+"/") {
			t.Errorf("restore path %q does not start with managed_root %q", rp, root)
		}
	}
}

func TestLoad_EmptyRestorePathsGetsDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"restore_paths": []string{},
	})
	policy, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.RestorePaths) == 0 {
		t.Error("empty restore_paths should fall back to defaults")
	}
}

// ── validate ──────────────────────────────────────────────────────────────────

// Default policy must pass validation — restore_paths /Users and
// /Applications don't include /var/lib/nivenia or /var/log.
func TestLoad_DefaultPolicyIsValid(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/System/Volumes/Data/Users", "/System/Volumes/Data/Applications"},
	})
	if _, err := Load(p); err != nil {
		t.Fatalf("default policy should be valid, got: %v", err)
	}
}

// A restore_path covering /var would wipe state.json on every boot,
// freezing the control plane. Must be rejected with a specific error.
func TestLoad_RejectsRestorePathOverWritingStateFile(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/System/Volumes/Data/var"},
	})
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected validation error when restore_path covers state file")
	}
	if !strings.Contains(err.Error(), "state_file") {
		t.Errorf("error should name the state_file conflict, got: %v", err)
	}
}

// Same risk if the admin writes the path without the explicit Data
// volume prefix. macOS firmlinks /var to /System/Volumes/Data/var, so
// rsync into "/var/lib/nivenia" from a snapshot mount would still hit
// the same files.
func TestLoad_RejectsFirmlinkEquivalentPath(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/System/Volumes/Data/var/lib"},
	})
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected validation error for /var/lib restore path")
	}
}

// log_file conflicts are equally dangerous — a wiped log file means
// the admin loses the trail back to the original failure.
func TestLoad_RejectsRestorePathOverWritingLogFile(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/System/Volumes/Data/var/log"},
		"log_file":      "/var/log/nivenia.log",
	})
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected validation error when restore_path covers log file")
	}
	if !strings.Contains(err.Error(), "log_file") {
		t.Errorf("error should name the log_file conflict, got: %v", err)
	}
}

// integrity.json must be excluded too — losing it forces a verify
// mismatch on every boot until refreeze.
func TestLoad_RejectsRestorePathOverWritingIntegrityFile(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/System/Volumes/Data/var/lib/nivenia"},
	})
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected validation error when restore_path is /var/lib/nivenia")
	}
}

// A restore_path outside managed_root is meaningless: rsync's source
// is a mount of managed_root, so paths outside resolve to nothing or
// (worse, with bad relative-path handling) to the wrong directory.
func TestLoad_RejectsRestorePathOutsideManagedRoot(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/System/Volumes/Data",
		"restore_paths": []string{"/some/other/volume/Users"},
	})
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected validation error for path outside managed_root")
	}
}

// Custom managed_root must still validate the same way: state files
// outside the managed root are safe (they won't be touched by rsync
// regardless), and restore paths must be inside the managed root.
func TestLoad_CustomManagedRootValidatesSimilarly(t *testing.T) {
	dir := t.TempDir()
	p := writeJSON(t, dir, map[string]any{
		"managed_root":  "/Volumes/Lab/Data",
		"restore_paths": []string{"/Volumes/Lab/Data/Users"},
		"state_file":    "/var/lib/nivenia/state.json",
		"log_file":      "/var/log/nivenia.log",
	})
	if _, err := Load(p); err != nil {
		t.Fatalf("custom managed_root with state outside it should be valid, got: %v", err)
	}
}
