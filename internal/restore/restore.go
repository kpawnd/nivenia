package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultSnapshotName = "nivenia-baseline"
	snapshotStatePath   = "/var/lib/nivenia/snapshot.json"
)

type snapshotState struct {
	Name         string `json:"name"`
	Volume       string `json:"volume"`
	CreatedAtUTC string `json:"created_at_utc"`
}

func SnapshotName() string {
	if state, ok := loadSnapshotState(); ok {
		return state.Name
	}
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return defaultSnapshotName
}

func desiredSnapshotName() string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return defaultSnapshotName
}

func loadSnapshotState() (snapshotState, bool) {
	data, err := os.ReadFile(snapshotStatePath)
	if err != nil {
		return snapshotState{}, false
	}
	var state snapshotState
	if err := json.Unmarshal(data, &state); err != nil {
		return snapshotState{}, false
	}
	if strings.TrimSpace(state.Name) == "" {
		return snapshotState{}, false
	}
	return state, true
}

func saveSnapshotState(volume, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("snapshot name is empty")
	}
	state := snapshotState{
		Name:         name,
		Volume:       volume,
		CreatedAtUTC: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(snapshotStatePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snapshotStatePath, data, 0o644)
}

func SnapshotVolume(managedRoot string) string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_VOLUME")); env != "" {
		return env
	}
	return managedRoot
}

func runDiskutil(args ...string) (string, error) {
	cmd := exec.Command("diskutil", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("diskutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runTmutil(args ...string) (string, error) {
	cmd := exec.Command("tmutil", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func diskutilAvailable() error {
	if _, err := exec.LookPath("diskutil"); err != nil {
		return fmt.Errorf("diskutil not found: %w", err)
	}
	return nil
}

func tmutilAvailable() error {
	if _, err := exec.LookPath("tmutil"); err != nil {
		return fmt.Errorf("tmutil not found: %w", err)
	}
	return nil
}

func isAPFSInfo(info string) bool {
	upper := strings.ToUpper(info)
	if strings.Contains(upper, "FILE SYSTEM PERSONALITY: APFS") {
		return true
	}
	if strings.Contains(upper, "TYPE (BUNDLE): APFS") {
		return true
	}
	if strings.Contains(upper, "APFS VOLUME") {
		return true
	}
	return false
}

func snapshotPreflight(volume, name string, requireSnapshot bool) error {
	if err := diskutilAvailable(); err != nil {
		return err
	}
	if strings.TrimSpace(volume) == "" {
		return fmt.Errorf("snapshot volume is empty")
	}
	if _, err := os.Stat(volume); err != nil {
		return fmt.Errorf("snapshot volume not found: %s: %w", volume, err)
	}
	info, err := runDiskutil("info", volume)
	if err != nil {
		return err
	}
	if !isAPFSInfo(info) {
		return fmt.Errorf("snapshot volume is not APFS: %s", strings.TrimSpace(info))
	}
	names, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return err
	}
	if requireSnapshot {
		found := false
		for _, existing := range names {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("snapshot %q not found on %s; available=%v", name, volume, names)
		}
	}
	return nil
}

func listAPFSSnapshotNames(volume string) ([]string, error) {
	out, err := runDiskutil("apfs", "listSnapshots", volume)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	var names []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Snapshot Name:"):
			name := strings.TrimSpace(strings.TrimPrefix(line, "Snapshot Name:"))
			if name != "" {
				names = append(names, name)
			}
		case strings.HasPrefix(line, "Name:"):
			name := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

func deleteAPFSSnapshot(volume, name string) error {
	_, err := runDiskutil("apfs", "deleteSnapshot", volume, "-name", name)
	return err
}

func createAPFSSnapshot(volume, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("snapshot name is empty")
	}
	if err := snapshotPreflight(volume, name, false); err != nil {
		return "", err
	}
	// Remove any existing snapshot with the same name to keep a single baseline.
	if existing, ok := loadSnapshotState(); ok {
		_ = deleteAPFSSnapshot(volume, existing.Name)
	} else if names, err := listAPFSSnapshotNames(volume); err == nil {
		for _, existing := range names {
			if existing == name {
				_ = deleteAPFSSnapshot(volume, name)
				break
			}
		}
	}

	if _, err := runDiskutil("apfs", "snapshot", volume, "-name", name); err == nil {
		return name, nil
	} else if !isSnapshotVerbUnsupported(err) {
		return "", err
	}

	fmt.Fprintln(os.Stderr, "[WARN] diskutil apfs snapshot unsupported; falling back to tmutil local snapshot")
	if err := tmutilAvailable(); err != nil {
		return "", err
	}

	before, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return "", err
	}
	var out string
	if out, err = runTmutil("localsnapshot"); err != nil {
		if out, err = runTmutil("snapshot"); err != nil {
			return "", err
		}
	}
	after, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return "", err
	}

	actual, err := findNewSnapshotName(before, after, out)
	if err != nil {
		return "", err
	}
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" && env != actual {
		fmt.Fprintln(os.Stderr, "[WARN] NIVENIA_SNAPSHOT_NAME ignored; tmutil snapshots are auto-named on this macOS version")
	}
	return actual, nil
}

// deviceForVolume returns the /dev/diskXsY path for the given volume mount path.
func deviceForVolume(volume string) (string, error) {
	out, err := runDiskutil("info", volume)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Device Identifier:") {
			id := strings.TrimSpace(strings.TrimPrefix(trimmed, "Device Identifier:"))
			if id != "" {
				return "/dev/" + id, nil
			}
		}
	}
	return "", fmt.Errorf("device identifier not found for %s", volume)
}

// mountSnapshotAt mounts a named APFS snapshot read-only at mountPoint.
// nobrowse hides it from Finder. mount_apfs is available on all macOS with APFS (10.12+).
func mountSnapshotAt(device, snapshotName, mountPoint string) error {
	cmd := exec.Command("mount_apfs", "-o", "nobrowse", "-s", snapshotName, device, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount_apfs -s %q %s: %w: %s", snapshotName, device, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rsyncRestore syncs src into dst, deleting files in dst that are absent in src.
// rsync exit 24 (vanished source files) is treated as non-fatal.
// Returns a short stats summary ("N files transferred").
func rsyncRestore(src, dst string) (string, error) {
	srcDir := strings.TrimRight(src, "/") + "/"
	dstDir := strings.TrimRight(dst, "/") + "/"
	cmd := exec.Command("rsync", "-aH", "--delete", "--force", "--stats", srcDir, dstDir)
	out, err := cmd.CombinedOutput()
	summary := parseRsyncStats(string(out))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 24 {
			return summary, nil
		}
		return summary, fmt.Errorf("rsync %s -> %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
	}
	return summary, nil
}

func parseRsyncStats(out string) string {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Number of regular files transferred:") {
			return t
		}
	}
	return ""
}

func CaptureBaseline(managedRoot string) error {
	volume := SnapshotVolume(managedRoot)
	actual, err := createAPFSSnapshot(volume, desiredSnapshotName())
	if err != nil {
		return err
	}
	return saveSnapshotState(volume, actual)
}

// RestoreFromBaseline mounts the frozen snapshot read-only and rsyncs each
// path in restorePaths from the snapshot back to the live volume.
// This works on all macOS versions (Monterey–Sequoia) without private entitlements.
func RestoreFromBaseline(managedRoot string, restorePaths []string) error {
	volume := SnapshotVolume(managedRoot)
	name := SnapshotName()

	fmt.Fprintf(os.Stderr, "[restore] snapshot: %s\n", name)

	if err := snapshotPreflight(volume, name, true); err != nil {
		return err
	}

	device, err := deviceForVolume(volume)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[restore] device: %s\n", device)

	mountPoint, err := os.MkdirTemp("", "nivenia-snap-*")
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer func() {
		_ = exec.Command("diskutil", "unmount", mountPoint).Run()
		_ = os.Remove(mountPoint)
	}()

	fmt.Fprintf(os.Stderr, "[restore] mounting snapshot...\n")
	if err := mountSnapshotAt(device, name, mountPoint); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[restore] mounted at %s\n", mountPoint)

	start := time.Now()
	for _, targetPath := range restorePaths {
		rel, err := filepath.Rel(managedRoot, targetPath)
		if err != nil {
			return fmt.Errorf("restore path %s: %w", targetPath, err)
		}
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("restore path %s is outside managed root %s", targetPath, managedRoot)
		}
		srcPath := filepath.Join(mountPoint, rel)
		if _, err := os.Stat(srcPath); err != nil {
			fmt.Fprintf(os.Stderr, "[restore] WARN: path not in snapshot, skipping: %s\n", targetPath)
			continue
		}
		t0 := time.Now()
		fmt.Fprintf(os.Stderr, "[restore] syncing %s...\n", targetPath)
		stats, err := rsyncRestore(srcPath, targetPath)
		if err != nil {
			return err
		}
		elapsed := time.Since(t0).Round(time.Millisecond)
		if stats != "" {
			fmt.Fprintf(os.Stderr, "[restore] done %s in %s (%s)\n", targetPath, elapsed, stats)
		} else {
			fmt.Fprintf(os.Stderr, "[restore] done %s in %s\n", targetPath, elapsed)
		}
	}
	fmt.Fprintf(os.Stderr, "[restore] completed in %s\n", time.Since(start).Round(time.Millisecond))

	return nil
}

func isSnapshotVerbUnsupported(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "did not recognize APFS verb \"snapshot\"")
}

func findNewSnapshotName(before, after []string, tmutilOut string) (string, error) {
	added := diffSnapshotNames(before, after)
	if len(added) == 1 {
		return added[0], nil
	}
	date := parseTmutilSnapshotDate(tmutilOut)
	if date != "" {
		for _, name := range added {
			if strings.Contains(name, date) {
				return name, nil
			}
		}
		for _, name := range after {
			if strings.Contains(name, date) {
				return name, nil
			}
		}
	}
	if len(added) > 0 {
		sort.Strings(added)
		return added[len(added)-1], nil
	}
	return "", fmt.Errorf("snapshot created but name not found for %s", strings.TrimSpace(tmutilOut))
}

func diffSnapshotNames(before, after []string) []string {
	seen := make(map[string]struct{}, len(before))
	for _, name := range before {
		seen[name] = struct{}{}
	}
	var added []string
	for _, name := range after {
		if _, ok := seen[name]; !ok {
			added = append(added, name)
		}
	}
	return added
}

func parseTmutilSnapshotDate(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "snapshot") && strings.Contains(trimmed, "date:") {
			parts := strings.SplitN(trimmed, "date:", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}
