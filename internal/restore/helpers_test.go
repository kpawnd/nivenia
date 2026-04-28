package restore

import (
	"strings"
	"testing"
)

// ── parseRsyncStats ───────────────────────────────────────────────────────────

func TestParseRsyncStats_Found(t *testing.T) {
	out := `
Number of files: 23181
Number of regular files transferred: 683
Total file size: 1281696986 B
`
	got := parseRsyncStats(out)
	want := "Number of regular files transferred: 683"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseRsyncStats_NotFound(t *testing.T) {
	out := "sent 235606274 bytes  received 17113 bytes"
	if got := parseRsyncStats(out); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestParseRsyncStats_Empty(t *testing.T) {
	if got := parseRsyncStats(""); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// ── isAPFSInfo ────────────────────────────────────────────────────────────────

func TestIsAPFSInfo_Positive(t *testing.T) {
	cases := []string{
		"File System Personality: APFS",
		"Type (Bundle): apfs",
		"APFS Volume Disk (Macintosh HD - Data)",
		"APFS VOLUME GROUP",
	}
	for _, c := range cases {
		if !isAPFSInfo(c) {
			t.Errorf("isAPFSInfo(%q) = false, want true", c)
		}
	}
}

func TestIsAPFSInfo_Negative(t *testing.T) {
	cases := []string{
		"File System Personality:  HFS+",
		"Type: ExFAT",
		"",
	}
	for _, c := range cases {
		if isAPFSInfo(c) {
			t.Errorf("isAPFSInfo(%q) = true, want false", c)
		}
	}
}

// ── parseSnapshotNames ────────────────────────────────────────────────────────
// Sequoia's diskutil emits the tree-style listing where each snapshot
// has a "Name:" line. We accept both "Name:" and "Snapshot Name:"
// because older diskutil versions used the longer form and we don't
// want to flake if Apple changes the formatting on a future point
// release.

func TestParseSnapshotNames_TreeStyle(t *testing.T) {
	output := `Snapshots for disk1s1 (1 found)
|
+-- E7A0A7FA-EA02-4397-8769-C66BE2E80A47
    Name:        com.apple.TimeMachine.2026-04-26-211521.local
    XID:         6704
    Purgeable:   Yes
    NOTE:        This snapshot limits the minimum size of APFS Container disk1
`
	names := parseSnapshotNames(output)
	if len(names) != 1 || names[0] != "com.apple.TimeMachine.2026-04-26-211521.local" {
		t.Fatalf("got %v, want [com.apple.TimeMachine.2026-04-26-211521.local]", names)
	}
}

func TestParseSnapshotNames_FlatFormat(t *testing.T) {
	// Older diskutil format (kept for forward compatibility).
	output := `
Snapshot Name:        com.apple.TimeMachine.2026-04-21-231314.local
XID:                  12345

Snapshot Name:        com.apple.TimeMachine.2026-04-22-031500.local
XID:                  67890
`
	names := parseSnapshotNames(output)
	if len(names) != 2 {
		t.Fatalf("got %d names %v, want 2", len(names), names)
	}
}

func TestParseSnapshotNames_NoSnapshots(t *testing.T) {
	if got := parseSnapshotNames("No snapshots found.\n"); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// ── snapshotDateRegex ─────────────────────────────────────────────────────────

func TestSnapshotDateRegex_ParsesTmutilOutput(t *testing.T) {
	out := "NOTE: local snapshots are considered purgeable and may be removed at any time by deleted(8).\nCreated local snapshot with date: 2026-04-26-223736"
	match := snapshotDateRegex.FindStringSubmatch(out)
	if len(match) < 2 {
		t.Fatalf("regex did not match tmutil output: %q", out)
	}
	if match[1] != "2026-04-26-223736" {
		t.Errorf("captured date: got %q, want 2026-04-26-223736", match[1])
	}
}

func TestSnapshotDateRegex_RejectsUnrelatedOutput(t *testing.T) {
	if snapshotDateRegex.FindStringSubmatch("some unrelated output") != nil {
		t.Error("regex matched something it shouldn't have")
	}
}

// ── classifyRsyncErrors ───────────────────────────────────────────────────────
// openrsync writes errors to stderr in the form
//   "rsync(PID): error: <details>"
// or
//   "rsync: error: <details>".
//
// Some of those lines come from a known race against macOS daemons
// (photolibraryd, akd, mds, ...) recreating cache directories during
// rsync's --delete phase — those are tolerable. Every other error
// line is fatal. classifyRsyncErrors splits them so the caller can
// decide whether to flip the boot into a failure.

// Captured verbatim from a production /var/log/nivenia logs run:
// rsync exit=23 with these five stderr lines and nothing else.
const productionDelRaceStderr = `rsync(368): error: admin/Library/Containers/com.apple.photolibraryd/Data/tmp: unlinkat: Directory not empty
rsync(368): error: admin/Library/Containers/com.apple.photolibraryd/Data: unlinkat: Directory not empty
rsync(368): error: admin/Library/Containers/com.apple.photolibraryd: unlinkat: Directory not empty
rsync(368): error: admin/Library/Caches/com.apple.akd: unlinkat: Directory not empty
rsync(368): error: ./admin/Library/Caches/com.apple.akd: unlinkat: Directory not empty
`

func TestClassifyRsyncErrors_AllDaemonRace(t *testing.T) {
	race, fatal := classifyRsyncErrors(productionDelRaceStderr)
	if len(race) != 5 {
		t.Errorf("expected 5 race lines, got %d: %v", len(race), race)
	}
	if len(fatal) != 0 {
		t.Errorf("expected 0 fatal lines, got %v", fatal)
	}
}

func TestClassifyRsyncErrors_FatalAlongsideRace(t *testing.T) {
	in := productionDelRaceStderr +
		"rsync: error: failed to read source file: permission denied\n"
	race, fatal := classifyRsyncErrors(in)
	if len(race) != 5 {
		t.Errorf("race lines: got %d, want 5", len(race))
	}
	if len(fatal) != 1 || !strings.Contains(fatal[0], "permission denied") {
		t.Errorf("fatal lines: got %v, want one permission-denied line", fatal)
	}
}

// AppleDouble openat ENOENT is the bug the previous commit's -E
// flag triggered. With -E removed those lines should never appear,
// but if they DO they are NOT a daemon race and must be classified
// as fatal so we don't silently ship a half-restore.
func TestClassifyRsyncErrors_AppleDoubleIsNotRace(t *testing.T) {
	in := "rsync(11858): error: Thorium.app/Contents/MacOS/._Thorium: openat: No such file or directory\n"
	race, fatal := classifyRsyncErrors(in)
	if len(race) != 0 {
		t.Errorf("race lines: got %v, want 0", race)
	}
	if len(fatal) != 1 {
		t.Errorf("fatal lines: got %d, want 1", len(fatal))
	}
}

func TestClassifyRsyncErrors_IgnoresNonErrorLines(t *testing.T) {
	in := strings.Join([]string{
		"Number of files: 1234",
		"sent 100 bytes  received 50 bytes",
		"warning: not an error",
		"",
		"some informational line",
	}, "\n")
	race, fatal := classifyRsyncErrors(in)
	if len(race) != 0 || len(fatal) != 0 {
		t.Errorf("clean stderr should produce zero buckets, got race=%v fatal=%v", race, fatal)
	}
}

// ── firstNonEmptyLines ────────────────────────────────────────────────────────

func TestFirstNonEmptyLines_TrimsAndCaps(t *testing.T) {
	in := "\n\n  rsync(1): error: a  \n   \nrsync(1): error: b\nrsync(1): error: c\nrsync(1): error: d\n"
	got := firstNonEmptyLines(in, 3)
	want := "rsync(1): error: a | rsync(1): error: b | rsync(1): error: c"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFirstNonEmptyLines_EmptyInput(t *testing.T) {
	if got := firstNonEmptyLines("", 5); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ── SnapshotName fallback ────────────────────────────────────────────────────

func TestSnapshotName_FallsBackToEnvWhenStateMissing(t *testing.T) {
	t.Setenv("NIVENIA_SNAPSHOT_NAME", "env-test-snapshot")
	if _, ok := loadSnapshotState(); ok {
		t.Skip("host has /var/lib/nivenia/snapshot.json — env-fallback can't be tested here")
	}
	if got := SnapshotName(); got != "env-test-snapshot" {
		t.Errorf("got %q, want env-test-snapshot", got)
	}
}
