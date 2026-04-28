// Package restore owns snapshot creation and snapshot-to-live
// restoration for the boot daemon.
//
// Sequoia-specific design notes (read these before changing anything):
//
//  1. There is no `diskutil apfs snapshot` verb on macOS. We previously
//     called one and it worked sometimes only because of a transient
//     diskutil bug; the verb is genuinely not exposed. Sequoia's only
//     supported way to create an APFS snapshot from userland is
//     `tmutil localsnapshot`, which auto-names the snapshot
//     `com.apple.TimeMachine.<UTC-date>.local`.
//
//     We do not get to choose the snapshot name. We parse the date out
//     of tmutil's stdout and verify the snapshot appears in
//     `diskutil apfs listSnapshots`. This is the single source of truth
//     for the new baseline; we then save the name to snapshot.json.
//
//  2. Sequoia ships `openrsync` as /usr/bin/rsync — a clean-room
//     reimplementation that calls itself "rsync 2.6.9 compatible".
//     Two things bit us hard with openrsync:
//
//     a. The `-E` flag (extended attributes via Apple's AppleDouble
//        scheme) causes openrsync to enumerate non-existent `._foo`
//        siblings for files that have xattrs, then explode with
//        `openat: No such file or directory` on those phantom files.
//        The transfer becomes partial AND openrsync exits 0, so the
//        old "trust the exit code" path silently shipped a half-restore.
//
//        Fix: drop -E. We accept the tradeoff that com.apple.quarantine
//        and Finder tags do not survive restore. For a lab Mac this is
//        acceptable; the snapshot itself is the curated baseline so the
//        apps in it are inherently trusted by the admin.
//
//     b. openrsync does NOT use the classic exit codes 23 (partial
//        transfer) and 24 (vanished source). Old code that special-cased
//        those was dead on Sequoia. The exit codes we actually see are
//        0 (success), 1 (syntax/usage), 2 (protocol), 5/10/11/12 (I/O
//        and protocol errors), 30 (timeout). All non-zero exits are
//        fatal; a zero exit is only success if stderr is also clean.
//
//  3. /Applications on Sequoia is a firmlink to /System/Volumes/Data/
//     Applications. The directory itself cannot be unlinkat'd by anyone,
//     including root (`Operation not permitted`). Any "wipe and recopy"
//     fallback is therefore wrong. rsync into the existing directory
//     works — it manipulates contents, not the firmlink target itself.
package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nivenia/internal/nivlog"
)

const (
	snapshotStatePath = "/var/lib/nivenia/snapshot.json"

	// snapshotNamePrefix is the prefix Time Machine uses for every
	// local snapshot it creates via `tmutil localsnapshot`. We don't
	// pick the name — TM does — but we recognise this prefix so we
	// can identify our snapshots when listing.
	tmSnapshotPrefix = "com.apple.TimeMachine."
	tmSnapshotSuffix = ".local"

	// listSnapshotRetries / listSnapshotDelay control the retry on the
	// "-69854 a disk with a mount point is required" race that occurs
	// during fast boots. Sequoia's diskutil sometimes returns this
	// transient error before APFS has fully attached the volume, even
	// after `diskutil info` reports success.
	listSnapshotRetries = 30
	listSnapshotDelay   = 5 * time.Second

	// mountSnapshotRetries handles the same family of transient errors
	// at mount_apfs time. Time Machine taking its own hourly snapshot
	// at the same instant can briefly contend.
	mountSnapshotRetries = 6
	mountSnapshotDelay   = 10 * time.Second
)

// snapshotDateRegex pulls the date string out of the tmutil
// localsnapshot success line: "Created local snapshot with date: 2026-04-26-223736".
var snapshotDateRegex = regexp.MustCompile(`(?m)^Created local snapshot with date:\s*([0-9-]+)\s*$`)

type snapshotState struct {
	Name         string `json:"name"`
	Volume       string `json:"volume"`
	CreatedAtUTC string `json:"created_at_utc"`
}

// SnapshotName returns the snapshot we should restore from at boot.
// Source of truth is /var/lib/nivenia/snapshot.json, which is written
// atomically once the snapshot is confirmed to exist.
func SnapshotName() string {
	if state, ok := loadSnapshotState(); ok {
		return state.Name
	}
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_NAME")); env != "" {
		return env
	}
	return ""
}

// SnapshotVolume picks the volume to snapshot/restore. NIVENIA_SNAPSHOT_VOLUME
// is honoured for tests and for hardware where the data volume is
// somewhere unusual; otherwise we use the policy's managed_root.
func SnapshotVolume(managedRoot string) string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_SNAPSHOT_VOLUME")); env != "" {
		return env
	}
	return managedRoot
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
	st := snapshotState{
		Name:         name,
		Volume:       volume,
		CreatedAtUTC: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(snapshotStatePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := snapshotStatePath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, snapshotStatePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ── External commands ─────────────────────────────────────────────────────────

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

// ── Volume helpers ────────────────────────────────────────────────────────────

func isAPFSInfo(info string) bool {
	upper := strings.ToUpper(info)
	return strings.Contains(upper, "FILE SYSTEM PERSONALITY: APFS") ||
		strings.Contains(upper, "TYPE (BUNDLE): APFS") ||
		strings.Contains(upper, "APFS VOLUME")
}

func isVolumeNotReady(err error) bool {
	return err != nil && strings.Contains(err.Error(), "-69854")
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

// ── Snapshot enumeration ──────────────────────────────────────────────────────

func listAPFSSnapshotNames(volume string) ([]string, error) {
	out, err := runDiskutil("apfs", "listSnapshots", volume)
	if err != nil {
		return nil, err
	}
	return parseSnapshotNames(out), nil
}

// parseSnapshotNames extracts snapshot names from `diskutil apfs
// listSnapshots` output. The format on Sequoia is the tree-style
// listing where each snapshot has a "Name:" line. We accept both
// "Name:" and "Snapshot Name:" because older diskutil versions used
// the longer form and we don't want to flake on a future variant.
func parseSnapshotNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Snapshot Name:"):
			n := strings.TrimSpace(strings.TrimPrefix(line, "Snapshot Name:"))
			if n != "" {
				names = append(names, n)
			}
		case strings.HasPrefix(line, "Name:"):
			n := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

// listSnapshotsWithRetry wraps the diskutil call with the -69854 retry
// loop. The retry only triggers on the specific transient error; any
// other error is returned immediately.
func listSnapshotsWithRetry(log *nivlog.Logger, volume string) ([]string, error) {
	var names []string
	var err error
	for attempt := 0; attempt < listSnapshotRetries; attempt++ {
		names, err = listAPFSSnapshotNames(volume)
		if err == nil {
			return names, nil
		}
		if !isVolumeNotReady(err) {
			return nil, err
		}
		if log != nil {
			log.Warn("snapshot.list.retry", "attempt", attempt+1, "error", err.Error())
		}
		time.Sleep(listSnapshotDelay)
	}
	return nil, err
}

func deleteAPFSSnapshot(volume, name string) error {
	_, err := runDiskutil("apfs", "deleteSnapshot", volume, "-name", name)
	return err
}

// ── Preflight ────────────────────────────────────────────────────────────────

// SnapshotPreflight verifies the volume is APFS, present, and
// (optionally) contains the named snapshot. Used by both the boot
// restore (with requireSnapshot=true) and the freeze flow (with
// requireSnapshot=false, since the snapshot doesn't exist yet).
func SnapshotPreflight(log *nivlog.Logger, volume, name string, requireSnapshot bool) error {
	if _, err := exec.LookPath("diskutil"); err != nil {
		return fmt.Errorf("diskutil not found: %w", err)
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
	names, err := listSnapshotsWithRetry(log, volume)
	if err != nil {
		return err
	}
	if requireSnapshot {
		for _, existing := range names {
			if existing == name {
				return nil
			}
		}
		return fmt.Errorf("snapshot %q not found on %s (available: %v)\n"+
			"  hint: snapshot may have been deleted by Time Machine pruning, "+
			"a macOS update, or storage pressure — run 'sudo niveniactl freeze' "+
			"to capture a new baseline", name, volume, names)
	}
	return nil
}

// ── Snapshot creation (freeze) ───────────────────────────────────────────────

// CaptureBaseline creates a fresh APFS snapshot via tmutil, persists
// its name to snapshot.json, then deletes the previous baseline.
//
// Atomic-swap rollback semantics:
//   - On success of all steps: snapshot.json points to the new
//     snapshot; the old one is deleted; system is fully migrated.
//   - On tmutil failure: nothing changed, error returned.
//   - On state-save failure: we attempt to delete the new snapshot
//     and return an error; the old snapshot remains live.
//   - On old-snapshot-delete failure: ignored. The new snapshot is
//     active; the old becomes a harmless orphan that next freeze
//     will clean up.
//
// At no point do we delete the old snapshot before the new one is
// confirmed to exist AND tracked in snapshot.json.
func CaptureBaseline(log *nivlog.Logger, managedRoot string) error {
	volume := SnapshotVolume(managedRoot)

	if err := SnapshotPreflight(log, volume, "", false); err != nil {
		return err
	}

	if log != nil {
		log.Info("freeze.start", "volume", volume)
	}

	// Take the snapshot. tmutil prints the date on success which we
	// use to derive the snapshot name (com.apple.TimeMachine.<date>.local).
	out, err := runTmutil("localsnapshot")
	if err != nil {
		if log != nil {
			log.Error("freeze.tmutil.fail", "error", err.Error(), "out", strings.TrimSpace(out))
		}
		return fmt.Errorf("tmutil localsnapshot: %w", err)
	}

	match := snapshotDateRegex.FindStringSubmatch(out)
	if len(match) < 2 {
		// Output didn't follow the expected format. Fall back to
		// listing — find any newly-appeared TM snapshot (best effort)
		// before returning the error so the admin has context.
		names, _ := listAPFSSnapshotNames(volume)
		if log != nil {
			log.Error("freeze.parse.fail", "tmutil_out", strings.TrimSpace(out), "snapshots", strings.Join(names, ","))
		}
		return fmt.Errorf("could not parse tmutil output: %q", strings.TrimSpace(out))
	}
	newName := tmSnapshotPrefix + match[1] + tmSnapshotSuffix

	// Verify the snapshot is visible before saving state. There's a
	// brief window where listSnapshots may not yet include the new one.
	visible := false
	for attempt := 0; attempt < 5; attempt++ {
		names, err := listAPFSSnapshotNames(volume)
		if err == nil {
			for _, n := range names {
				if n == newName {
					visible = true
					break
				}
			}
		}
		if visible {
			break
		}
		time.Sleep(time.Second)
	}
	if !visible {
		if log != nil {
			log.Error("freeze.verify.fail", "expected", newName)
		}
		return fmt.Errorf("created snapshot %q not visible in listSnapshots", newName)
	}

	if log != nil {
		log.Info("freeze.snapshot.created", "name", newName)
	}

	// Capture the previous baseline name BEFORE saving the new state
	// (saveSnapshotState will overwrite snapshot.json).
	previous, hadPrevious := loadSnapshotState()

	if err := saveSnapshotState(volume, newName); err != nil {
		// Rollback: delete the snapshot we just made so we don't
		// leak it. The old snapshot is still pointed to by the
		// previous (now-unchanged) snapshot.json.
		_ = deleteAPFSSnapshot(volume, newName)
		if log != nil {
			log.Error("freeze.state.save.fail", "error", err.Error())
		}
		return fmt.Errorf("save snapshot state: %w (rolled back)", err)
	}

	// State successfully points at the new snapshot. Reap the old
	// baseline, but ONLY if it's a Nivenia/TM snapshot we tracked —
	// never delete an arbitrary user snapshot.
	if hadPrevious && previous.Name != "" && previous.Name != newName {
		if err := deleteAPFSSnapshot(volume, previous.Name); err != nil {
			if log != nil {
				log.Warn("freeze.old.delete.fail", "name", previous.Name, "error", err.Error())
			}
		} else if log != nil {
			log.Info("freeze.old.deleted", "name", previous.Name)
		}
	}

	if log != nil {
		log.Info("freeze.complete", "snapshot", newName)
	}
	return nil
}

// ── Mount snapshot ────────────────────────────────────────────────────────────

// isTransientMountError detects errors mount_apfs may emit during the
// boot window when APFS is still settling, or when Time Machine is
// taking a snapshot at the same time.
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
// nobrowse hides it from Finder. Retries on transient errors that
// occur during the fast-boot window.
func mountSnapshotAt(log *nivlog.Logger, device, snapshotName, mountPoint string) error {
	var lastErr error
	for attempt := 0; attempt < mountSnapshotRetries; attempt++ {
		cmd := exec.Command("mount_apfs", "-o", "nobrowse", "-s", snapshotName, device, mountPoint)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("mount_apfs -s %q %s: %w: %s", snapshotName, device, err, strings.TrimSpace(string(out)))
		if !isTransientMountError(lastErr) {
			return lastErr
		}
		if log != nil {
			log.Warn("snapshot.mount.retry", "attempt", attempt+1, "error", lastErr.Error())
		}
		time.Sleep(mountSnapshotDelay)
	}
	return lastErr
}

// ── rsync ─────────────────────────────────────────────────────────────────────

// rsyncErrorLineRE matches an openrsync error line:
//
//	rsync(12345): error: <details>
//	rsync: error: <details>
//
// A literal "error:" token in the output is openrsync's documented
// signal of a real failure (or, for the daemon-race case below, a
// recoverable cleanup failure — we classify lines further once we
// know they're errors).
var rsyncErrorLineRE = regexp.MustCompile(`^rsync(?:\([0-9]+\))?:\s*error:`)

// rsyncRaceErrorRE matches stderr error lines from openrsync's
// `--delete` cleanup phase that lost a race against a macOS daemon
// (photolibraryd, akd, mds, ...) recreating files in a directory
// rsync was trying to remove. Observed on every Sequoia run that
// has a logged-in user, captured verbatim from production logs:
//
//	rsync(368): error: admin/Library/Containers/com.apple.photolibraryd/Data/tmp: unlinkat: Directory not empty
//	rsync(368): error: admin/Library/Caches/com.apple.akd: unlinkat: Directory not empty
//
// These directories live OUTSIDE the snapshot (the daemon created
// them post-freeze) and contain only daemon-managed cache files
// that are rebuilt on demand. The transfer phase still completed
// — every file from the snapshot is on the live volume. The only
// thing the race left behind is a daemon's freshly-rewritten cache
// directory that we couldn't unlink in time.
//
// For boot-restore semantics — "the live volume should look like
// the snapshot for everything we care about" — this is acceptable:
// the file content from the snapshot is fully in place; some
// daemon-managed sibling directories survive. We surface it as a
// WARN-level structured event ("rsync.partial.delete_race") so an
// admin can still see the names of the directories that lost the
// race, but we don't flip the boot into a failure mode.
//
// Anchor on the message tail because openrsync prefixes the error
// with the relative path; we don't want to be tied to any specific
// daemon name.
var rsyncRaceErrorRE = regexp.MustCompile(`unlinkat:\s*Directory not empty\s*$`)

// classifyRsyncErrors splits stderr lines that match the rsync
// "error:" prefix into two buckets:
//
//   - race: lines whose tail is "unlinkat: Directory not empty",
//     i.e. the daemon-rebuild cleanup race documented above.
//     Tolerable.
//   - fatal: every other rsync error line. Intolerable; even one
//     of these means the restore did not complete cleanly and the
//     caller must fail the boot.
//
// Non-error lines (stats, warnings, blank) are ignored.
func classifyRsyncErrors(stderr string) (race []string, fatal []string) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !rsyncErrorLineRE.MatchString(line) {
			continue
		}
		if rsyncRaceErrorRE.MatchString(line) {
			race = append(race, line)
		} else {
			fatal = append(fatal, line)
		}
	}
	return
}

// rsyncResult is the parsed outcome of one rsync invocation. We carry
// stdout AND stderr separately because openrsync's error reporting
// goes through stderr and was being lost when we used CombinedOutput.
type rsyncResult struct {
	exitCode  int    // -1 if cmd.Run errored without producing one
	stdout    string // captured stdout (rsync stats, file list)
	stderr    string // captured stderr (errors, warnings)
	statsLine string // pre-parsed "Number of regular files transferred: N" line
	cmdline   string // rendered command for the log
}

// runRsync executes rsync with the given arguments and captures stdout
// and stderr separately. It returns the result struct unconditionally;
// the caller decides whether to treat the run as success or failure.
func runRsync(ctx context.Context, args []string) rsyncResult {
	cmd := exec.CommandContext(ctx, "rsync", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()

	res := rsyncResult{
		stdout:  outBuf.String(),
		stderr:  errBuf.String(),
		cmdline: "rsync " + strings.Join(args, " "),
	}
	res.statsLine = parseRsyncStats(res.stdout)

	if err == nil {
		res.exitCode = 0
		return res
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.exitCode = exitErr.ExitCode()
		return res
	}
	// Couldn't even start rsync, or context cancellation killed it.
	res.exitCode = -1
	if ctx.Err() != nil {
		res.stderr = strings.TrimRight(res.stderr, "\n") + "\nrsync: cancelled by context: " + ctx.Err().Error()
	} else {
		res.stderr = strings.TrimRight(res.stderr, "\n") + "\nrsync: " + err.Error()
	}
	return res
}

// parseRsyncStats picks the "files transferred" stats line out of
// rsync's --stats output. Apple's classic rsync 2.6.9 wrote
//
//	Number of regular files transferred: 683
//
// Sequoia's openrsync writes
//
//	Number of files transferred: 0
//
// We accept either form so the same code works through any future
// switch. The line is reported verbatim to the structured log so
// admins can see whether a restore actually moved anything (a busy
// /Applications restore should always be > 0; a tight rerun is 0).
func parseRsyncStats(out string) string {
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "Number of regular files transferred:") {
			return t
		}
		if strings.HasPrefix(t, "Number of files transferred:") {
			return t
		}
	}
	return ""
}

// rsyncRestore syncs src into dst, deleting files in dst that are
// absent from src. Flags chosen for Sequoia/openrsync compatibility:
//
//	-a   archive (recursive, perms, times, owner, group, symlinks)
//	-H   preserve hard links
//	     (no -E: see file-level comment for the openrsync bug it triggers)
//
// Returns a short summary on success. On failure, returns an error
// whose message includes the exit code, the cmdline, and a reference
// to the per-run stderr file (written by the caller via WriteDetail).
func rsyncRestore(ctx context.Context, log *nivlog.Logger, src, dst string) (string, error) {
	srcDir := strings.TrimRight(src, "/") + "/"
	dstDir := strings.TrimRight(dst, "/") + "/"

	args := []string{
		"-aH",
		"--delete",
		"--stats",
		// Spotlight metadata uses cross-directory hardlinks rsync
		// can't replicate cleanly. Spotlight rebuilds them on boot.
		"--exclude=.Spotlight-V100",
		"--exclude=.fseventsd",
		"--exclude=CoreSpotlight",
		// AppleDouble shadow files (._foo). On APFS these are
		// virtual and rsync's enumeration of them has historically
		// produced spurious openat ENOENT errors. We don't need
		// them since we don't request -E.
		"--exclude=._*",
		srcDir, dstDir,
	}

	res := runRsync(ctx, args)

	// Write the full rsync transcript to a per-run detail file
	// regardless of outcome, so the admin always has the trail.
	detailPath := ""
	if log != nil {
		body := fmt.Sprintf("# %s\n# exit=%d\n# === stdout ===\n%s\n# === stderr ===\n%s",
			res.cmdline, res.exitCode, res.stdout, res.stderr)
		detailPath = log.WriteDetail("rsync-"+filepath.Base(dstDir), body)
	}

	race, fatal := classifyRsyncErrors(res.stderr)

	// Clean success: rsync exited 0 and stderr is silent (or only
	// contained warnings we don't recognise, which we still ignore).
	if res.exitCode == 0 && len(race) == 0 && len(fatal) == 0 {
		if log != nil {
			log.Info("rsync.ok",
				"src", srcDir,
				"dst", dstDir,
				"stats", res.statsLine,
				"detail", detailPath,
			)
		}
		return res.statsLine, nil
	}

	// Tolerated partial: the transfer succeeded but `--delete`
	// cleanup lost a race against a still-running macOS daemon.
	// rsync exits 0 (when the race produces no openrsync errors)
	// or 23 (when it does). We accept either, BUT only when every
	// stderr error line is the daemon-race pattern. A single fatal
	// error in the same batch tips the whole run into failure —
	// we don't silently mix partial cleanup with real corruption.
	if (res.exitCode == 0 || res.exitCode == 23) && len(fatal) == 0 && len(race) > 0 {
		if log != nil {
			log.Warn("rsync.partial.delete_race",
				"src", srcDir,
				"dst", dstDir,
				"exit", res.exitCode,
				"skipped_dirs", len(race),
				"sample", firstNonEmptyLines(strings.Join(race, "\n"), 3),
				"stats", res.statsLine,
				"detail", detailPath,
			)
		}
		return res.statsLine, nil
	}

	// Genuine failure. Build an error message that's both grep-
	// friendly in the main log AND points to the detail file for
	// the full transcript. We deliberately do NOT treat any
	// non-zero exit as success blindly — that hid the openrsync
	// half-restore bug we already paid for.
	combined := append([]string{}, fatal...)
	combined = append(combined, race...)
	if len(combined) == 0 {
		combined = []string{strings.TrimSpace(res.stderr)}
	}
	stderrSnippet := firstNonEmptyLines(strings.Join(combined, "\n"), 5)
	if log != nil {
		log.Error("rsync.fail",
			"src", srcDir,
			"dst", dstDir,
			"exit", res.exitCode,
			"fatal_count", len(fatal),
			"race_count", len(race),
			"stderr_summary", stderrSnippet,
			"detail", detailPath,
		)
	}
	return res.statsLine, fmt.Errorf("rsync %s -> %s: exit=%d (detail=%s) stderr: %s",
		srcDir, dstDir, res.exitCode, detailPath, stderrSnippet)
}

func firstNonEmptyLines(s string, n int) string {
	var picked []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		picked = append(picked, line)
		if len(picked) >= n {
			break
		}
	}
	if len(picked) == 0 {
		return ""
	}
	return strings.Join(picked, " | ")
}

// ── Boot restore ──────────────────────────────────────────────────────────────

// RestoreFromBaseline mounts the saved snapshot read-only and rsyncs
// each restorePath from snapshot to live. ctx cancellation kills the
// in-flight rsync (so a SIGTERM to niveniad doesn't leave orphans).
func RestoreFromBaseline(ctx context.Context, log *nivlog.Logger, managedRoot string, restorePaths []string) error {
	volume := SnapshotVolume(managedRoot)
	name := SnapshotName()
	if log != nil {
		log.Info("restore.preflight.start", "volume", volume, "snapshot", name)
	}

	if err := SnapshotPreflight(log, volume, name, true); err != nil {
		if log != nil {
			log.Error("restore.preflight.fail", "error", err.Error())
		}
		return err
	}

	device, err := deviceForVolume(volume)
	if err != nil {
		if log != nil {
			log.Error("restore.device.fail", "error", err.Error())
		}
		return err
	}

	mountPoint, err := os.MkdirTemp("", "nivenia-snap-*")
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer func() {
		// Try clean unmount; force-unmount as a fallback so a
		// dangling snapshot mount can never block the next boot.
		if uErr := exec.Command("diskutil", "unmount", mountPoint).Run(); uErr != nil {
			_ = exec.Command("diskutil", "unmount", "force", mountPoint).Run()
		}
		_ = os.Remove(mountPoint)
	}()

	if log != nil {
		log.Info("restore.mount.start", "device", device, "snapshot", name, "mountpoint", mountPoint)
	}
	if err := mountSnapshotAt(log, device, name, mountPoint); err != nil {
		if log != nil {
			log.Error("restore.mount.fail", "error", err.Error())
		}
		return err
	}
	if log != nil {
		log.Info("restore.mount.ok", "mountpoint", mountPoint)
	}

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
			if log != nil {
				log.Warn("restore.path.absent", "path", targetPath, "stat_error", err.Error())
			}
			continue
		}

		if log != nil {
			log.Info("rsync.start", "src", srcPath, "dst", targetPath)
		}
		t0 := time.Now()
		stats, err := rsyncRestore(ctx, log, srcPath, targetPath)
		elapsed := time.Since(t0).Round(time.Millisecond)
		if err != nil {
			if log != nil {
				log.Error("restore.path.fail",
					"path", targetPath,
					"elapsed", elapsed.String(),
					"error", err.Error(),
				)
			}
			return err
		}
		if log != nil {
			log.Info("restore.path.ok",
				"path", targetPath,
				"elapsed", elapsed.String(),
				"stats", stats,
			)
		}
	}
	if log != nil {
		log.Info("restore.complete", "elapsed", time.Since(start).Round(time.Millisecond).String())
	}
	return nil
}
