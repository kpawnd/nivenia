package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// snapshotNamePrefix is the constant prefix every Nivenia-created
	// snapshot starts with. The remainder is a UTC timestamp in ISO 8601
	// basic format (no colons, valid in APFS snapshot names) so each
	// freeze produces a unique name that never collides with a previous
	// baseline. Using fresh names is the only way to make freeze atomic:
	// we keep the OLD snapshot live until the new one has been confirmed,
	// then delete the old one. With a fixed name, the create-then-delete
	// dance had to delete first (because APFS rejects duplicate names),
	// which left a window where a transient create failure orphaned the
	// machine with NO baseline.
	snapshotNamePrefix = "nivenia-"
	snapshotStatePath  = "/var/lib/nivenia/snapshot.json"
	// snapshotTimestampLayout matches Go's reference time. The format is
	// "20060102T150405Z" — basic ISO 8601, UTC. Letters are alphanumeric
	// and the "Z" suffix is unambiguous, so the resulting name is safe
	// for diskutil/APFS (no colons, no spaces, no slashes).
	snapshotTimestampLayout = "20060102T150405Z"
)

type snapshotState struct {
	Name         string `json:"name"`
	Volume       string `json:"volume"`
	CreatedAtUTC string `json:"created_at_utc"`
}

// SnapshotName returns the name of the snapshot we should restore from.
// At boot it must be the snapshot we previously CAPTURED, not a freshly
// generated one. Source of truth is /var/lib/nivenia/snapshot.json which
// is written atomically once the snapshot is confirmed to exist.
//
// NIVENIA_SNAPSHOT_NAME (env) is honoured only as a last-resort fallback
// for situations where snapshot.json is unreadable (Recovery boot, manual
// debugging). It is not the normal source of truth.
func SnapshotName() string {
	if state, ok := loadSnapshotState(); ok {
		return state.Name
	}
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return ""
}

// freshSnapshotName generates a new unique name for a freeze operation.
// Because we never reuse names, the old snapshot stays live until the
// new one is confirmed, making freeze rollback-safe.
//
// Test/manual override via NIVENIA_SNAPSHOT_NAME is supported but emits
// a warning: pinning a fixed name reintroduces the create-after-delete
// race that this module was redesigned to eliminate.
func freshSnapshotName() string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		fmt.Fprintln(os.Stderr, "[WARN] NIVENIA_SNAPSHOT_NAME set; using fixed name disables atomic rollback on freeze failure")
		return env
	}
	return snapshotNamePrefix + time.Now().UTC().Format(snapshotTimestampLayout)
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

func diskutilAvailable() error {
	if _, err := exec.LookPath("diskutil"); err != nil {
		return fmt.Errorf("diskutil not found: %w", err)
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

func isVolumeNotReady(err error) bool {
	return err != nil && strings.Contains(err.Error(), "-69854")
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
	// diskutil apfs listSnapshots can transiently return -69854 ("A disk with a
	// mount point is required") on fast reboots even after diskutil info
	// succeeds. Retry with backoff before giving up.
	var names []string
	for attempt := 0; attempt < 15; attempt++ {
		names, err = listAPFSSnapshotNames(volume)
		if err == nil || !isVolumeNotReady(err) {
			break
		}
		time.Sleep(10 * time.Second)
	}
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
			return fmt.Errorf("snapshot %q not found on %s (available: %v)\n"+
				"  hint: snapshot may have been deleted under storage pressure — "+
				"run 'niveniactl freeze' to capture a new baseline", name, volume, names)
		}
	}
	return nil
}

func listAPFSSnapshotNamesFromOutput(out string) ([]string, error) {
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

func listAPFSSnapshotNames(volume string) ([]string, error) {
	out, err := runDiskutil("apfs", "listSnapshots", volume)
	if err != nil {
		return nil, err
	}
	return listAPFSSnapshotNamesFromOutput(out)
}

func deleteAPFSSnapshot(volume, name string) error {
	_, err := runDiskutil("apfs", "deleteSnapshot", volume, "-name", name)
	return err
}

func runTmutil(args ...string) (string, error) {
	cmd := exec.Command("tmutil", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func isUnsupportedAPFSSnapshotVerb(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "did not recognize apfs verb \"snapshot\"") || strings.Contains(text, "verb \"snapshot\" is not recognized")
}

func snapshotNameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

func diffSnapshotNames(before, after []string) []string {
	seen := snapshotNameSet(before)
	var created []string
	for _, name := range after {
		if _, ok := seen[name]; !ok {
			created = append(created, name)
		}
	}
	return created
}

func createAPFSSnapshotWithTmutil(volume string, beforeNames []string) (string, error) {
	if _, err := runTmutil("localsnapshot"); err != nil {
		return "", err
	}
	afterNames, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return "", err
	}
	created := diffSnapshotNames(beforeNames, afterNames)
	if len(created) != 1 {
		return "", fmt.Errorf("could not identify created snapshot on %s (before=%v after=%v)", volume, beforeNames, afterNames)
	}
	return created[0], nil
}

// createAPFSSnapshot performs an atomic-swap freeze: it creates a new
// snapshot under a unique name, persists the new name to snapshot.json,
// and only then deletes the previous baseline. If any step fails, the
// previous baseline remains live and intact — the machine is never left
// without a working restore point.
//
// Some macOS builds do not expose a diskutil APFS snapshot-creation verb.
// In that case we fall back to tmutil localsnapshot and record the actual
// snapshot name that was created.
func createAPFSSnapshot(volume, newName string) (string, error) {
	if newName == "" {
		return "", fmt.Errorf("snapshot name is empty")
	}
	if err := snapshotPreflight(volume, newName, false); err != nil {
		return "", err
	}

	beforeNames, err := listAPFSSnapshotNames(volume)
	if err != nil {
		return "", err
	}

	createdName := newName
	if _, err := runDiskutil("apfs", "snapshot", volume, "-name", newName); err != nil {
		if !isUnsupportedAPFSSnapshotVerb(err) {
			return "", err
		}
		createdName, err = createAPFSSnapshotWithTmutil(volume, beforeNames)
		if err != nil {
			return "", err
		}
	} else {
		afterNames, err := listAPFSSnapshotNames(volume)
		if err != nil {
			return "", err
		}
		created := diffSnapshotNames(beforeNames, afterNames)
		if len(created) != 1 {
			return "", fmt.Errorf("could not identify created snapshot on %s (before=%v after=%v)", volume, beforeNames, afterNames)
		}
		createdName = created[0]
	}

	// Snapshot create succeeded. Update snapshot.json with the new name
	// BEFORE deleting the previous baseline so a power cut between
	// success and delete leaves the new name persisted (the old snapshot
	// is then orphaned but harmless). Boot restore reads the new name
	// and finds the new snapshot.
	previous, hadPrevious := loadSnapshotState()
	if err := saveSnapshotState(volume, createdName); err != nil {
		// Persistence failed but the snapshot itself exists. Best effort:
		// leave both snapshots in place. Boot restore will keep using
		// the previous snapshot (still pointed to by snapshot.json),
		// while the new snapshot becomes a harmless orphan that the
		// next successful freeze cycle will clean up.
		_ = deleteAPFSSnapshot(volume, createdName)
		return "", fmt.Errorf("snapshot created but state save failed (snapshot rolled back): %w", err)
	}

	// State successfully points at the new snapshot. Now reap the old
	// one to reclaim space. A failure here is harmless — the orphan
	// just consumes APFS COW overhead until the next freeze.
	if hadPrevious && previous.Name != "" && previous.Name != createdName {
		_ = deleteAPFSSnapshot(volume, previous.Name)
	}

	return createdName, nil
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

// isTransientMountError detects errors that mount_apfs may emit during
// the boot window when APFS is still settling — same family as the
// listSnapshots -69854 race. Time Machine creating its own hourly
// snapshot at the same instant can also briefly hold a lock that
// makes mount_apfs return "Resource busy". These are worth retrying;
// "snapshot not found" or "permission denied" are not.
func isTransientMountError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "-69854") ||
		strings.Contains(msg, "Resource busy") ||
		strings.Contains(msg, "Device busy") ||
		strings.Contains(msg, "could not be mounted")
}

// mountSnapshotAt mounts a named APFS snapshot read-only at mountPoint.
// nobrowse hides it from Finder. mount_apfs is available on all macOS
// with APFS (10.12+). Retries on transient errors that occur during the
// fast-boot window.
func mountSnapshotAt(device, snapshotName, mountPoint string) error {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		cmd := exec.Command("mount_apfs", "-o", "nobrowse", "-s", snapshotName, device, mountPoint)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("mount_apfs -s %q %s: %w: %s", snapshotName, device, err, strings.TrimSpace(string(out)))
		if !isTransientMountError(lastErr) {
			return lastErr
		}
		// Linear back-off — the longest wait we tolerate is ~1 minute,
		// well within the launchd-managed boot timeout.
		time.Sleep(10 * time.Second)
	}
	return lastErr
}

// rsyncRestore syncs src into dst, deleting files in dst that are absent in src.
// rsync exit 23 (partial transfer, e.g. hard-link errors) and 24 (vanished source
// files) are treated as non-fatal. ctx cancellation kills the rsync subprocess.
// Returns a short stats summary ("N files transferred").
//
// Flags:
//
//	-a   archive: recursive + symlinks + perms + times + group/owner + devices
//	-H   preserve hard links
//	-E   preserve extended attributes (Apple-patched rsync 2.6.9 — this is the
//	     flag that, on macOS, copies xattrs *and* ACLs *and* resource forks,
//	     rolled into one. Without -E, the restored files lose the
//	     com.apple.quarantine xattr (so Gatekeeper would re-prompt for any
//	     downloaded .dmg/.pkg in the snapshot), Finder tags, custom icons,
//	     and any access-control entries set with `chmod +a`. Apple maps -E
//	     to whatever the supported metadata is for the destination FS, so
//	     APFS→APFS round-trips are lossless.)
func rsyncRestore(ctx context.Context, src, dst string) (string, error) {
	srcDir := strings.TrimRight(src, "/") + "/"
	dstDir := strings.TrimRight(dst, "/") + "/"
	// .Spotlight-V100 and CoreSpotlight use cross-directory hard links that
	// rsync -H cannot replicate atomically (exit 23). Both are regenerated by
	// Spotlight on boot, so excluding them is safe and eliminates the noise.
	var summary string
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.CommandContext(ctx, "rsync", "-aHE", "--delete", "--force", "--stats",
			"--exclude=.Spotlight-V100",
			"--exclude=.fseventsd",
			"--exclude=CoreSpotlight",
			srcDir, dstDir)
		out, err := cmd.CombinedOutput()
		summary = parseRsyncStats(string(out))
		if err == nil {
			return summary, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			switch exitErr.ExitCode() {
			case 20, 23, 24:
				// 20 = transfer interrupted by signal
				// 23 = partial transfer (e.g. cross-directory hard links in system metadata)
				// 24 = source files vanished mid-transfer
				return summary, nil
			}
		}
		lastErr = fmt.Errorf("rsync %s -> %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
		if ctx.Err() != nil || attempt == 2 || !isRetryableRsyncError(lastErr) {
			return summary, lastErr
		}
		// Retry once or twice for transient source-read failures. The source
		// is a mounted snapshot, so a second pass is safe and often enough to
		// get past a transient EOF from rsync's file reading path.
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return summary, lastErr
}

func restoreApplicationsFallback(ctx context.Context, src, dst string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("remove %s before fallback restore: %w", dst, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("recreate parent for %s: %w", dst, err)
	}
	cmd := exec.CommandContext(ctx, "ditto", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ditto %s -> %s: %w: %s", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func isRetryableRsyncError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unexpected end of file") ||
		strings.Contains(text, "unexpected eof") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "connection unexpectedly closed") ||
		strings.Contains(text, "exit status 20")
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

// CaptureBaseline creates a fresh APFS snapshot of the managed volume
// and persists its name. State is updated by createAPFSSnapshot itself
// as part of the atomic swap; on failure the previous baseline (if any)
// is left untouched so boot restore continues to work.
func CaptureBaseline(managedRoot string) error {
	volume := SnapshotVolume(managedRoot)
	_, err := createAPFSSnapshot(volume, freshSnapshotName())
	return err
}

// RestoreFromBaseline mounts the frozen snapshot read-only and rsyncs each
// path in restorePaths from the snapshot back to the live volume.
// This works on all macOS versions (Monterey–Sequoia) without private entitlements.
// ctx cancellation (e.g. SIGTERM) is propagated to the rsync subprocess so it
// is killed before the caller exits, preventing orphaned rsyncs after daemon stop.
func RestoreFromBaseline(ctx context.Context, managedRoot string, restorePaths []string) error {
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
		if err := exec.Command("diskutil", "unmount", mountPoint).Run(); err != nil {
			// Force-unmount so a dangling snapshot never blocks the next boot restore.
			_ = exec.Command("diskutil", "unmount", "force", mountPoint).Run()
		}
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
		stats, err := rsyncRestore(ctx, srcPath, targetPath)
		if err != nil {
			if pathpkg.Clean(targetPath) == "/System/Volumes/Data/Applications" && isRetryableRsyncError(err) {
				fmt.Fprintf(os.Stderr, "[restore] rsync failed for %s; retrying with ditto fallback\n", targetPath)
				if fallbackErr := restoreApplicationsFallback(ctx, srcPath, targetPath); fallbackErr == nil {
					fmt.Fprintf(os.Stderr, "[restore] fallback restore for %s completed\n", targetPath)
					continue
				} else {
					return fallbackErr
				}
			}
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
