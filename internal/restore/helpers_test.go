package restore

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
	got := parseRsyncStats(out)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestParseRsyncStats_Empty(t *testing.T) {
	if got := parseRsyncStats(""); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestIsRetryableRsyncError(t *testing.T) {
	cases := []string{
		"rsync: error: unexpected end of file",
		"rsync: error: unexpected EOF",
		"rsync: error: broken pipe",
		"rsync: error: connection unexpectedly closed",
		"rsync: exit status 20",
	}
	for _, msg := range cases {
		if !isRetryableRsyncError(fmt.Errorf(msg)) {
			t.Fatalf("isRetryableRsyncError(%q) = false, want true", msg)
		}
	}
	if isRetryableRsyncError(fmt.Errorf("rsync: error: permission denied")) {
		t.Fatal("permission denied should not be retryable")
	}
}

// ── isAPFSInfo ────────────────────────────────────────────────────────────────

func TestIsAPFSInfo_Positive(t *testing.T) {
	cases := []string{
		// Single-space variant matches the substring check literally.
		"File System Personality: APFS",
		"Type (Bundle): apfs",
		// Multi-word APFS Volume substring is the reliable cross-version match.
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

// ── listAPFSSnapshotNames ─────────────────────────────────────────────────────

func TestListAPFSSnapshotNames_ParsesOutput(t *testing.T) {
	// Flat format as produced by diskutil apfs listSnapshots on macOS Sonoma.
	output := `
Snapshot Name:        com.apple.TimeMachine.2026-04-21-231314.local
Snapshot UUID:        AABBCCDD-1234-5678-90AB-CDEF01234567
XID:                  12345
Timestamp:            2026-04-21 23:13:14 +0000

Snapshot Name:        nivenia-baseline
Snapshot UUID:        11223344-AAAA-BBBB-CCCC-DDDDEEEEFFFF
XID:                  67890
Timestamp:            2026-04-21 00:00:00 +0000
`
	names, err := listAPFSSnapshotNamesFromOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names %v, want 2", len(names), names)
	}
	if names[0] != "com.apple.TimeMachine.2026-04-21-231314.local" {
		t.Errorf("names[0]: got %q", names[0])
	}
	if names[1] != "nivenia-baseline" {
		t.Errorf("names[1]: got %q", names[1])
	}
}

func TestListAPFSSnapshotNames_OlderFormat(t *testing.T) {
	output := `
Name:   snap-one
Name:   snap-two
`
	names, err := listAPFSSnapshotNamesFromOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names %v, want 2", len(names), names)
	}
}

func TestListAPFSSnapshotNames_Empty(t *testing.T) {
	names, err := listAPFSSnapshotNamesFromOutput("No snapshots found.\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Errorf("got %v, want empty", names)
	}
}

// ── snapshot fallback helpers ───────────────────────────────────────────────

func TestDiffSnapshotNames_FindsNewEntries(t *testing.T) {
	before := []string{"snap-a", "snap-b"}
	after := []string{"snap-a", "snap-b", "snap-c"}
	created := diffSnapshotNames(before, after)
	if len(created) != 1 || created[0] != "snap-c" {
		t.Fatalf("diffSnapshotNames() = %v, want [snap-c]", created)
	}
}

func TestUnsupportedAPFSSnapshotVerb_RecognizesDiskutilError(t *testing.T) {
	err := fmt.Errorf(`diskutil apfs snapshot /System/Volumes/Data -name nivenia: exit status 1: diskutil: did not recognize APFS verb "snapshot"; type "diskutil apfs" for a list`)
	if !isUnsupportedAPFSSnapshotVerb(err) {
		t.Fatal("expected unsupported APFS snapshot verb to be recognized")
	}
}

// ── freshSnapshotName ─────────────────────────────────────────────────────────

func TestFreshSnapshotName_HasPrefixAndTimestamp(t *testing.T) {
	t.Setenv("NIVENIA_SNAPSHOT_NAME", "")
	got := freshSnapshotName()
	if !strings.HasPrefix(got, snapshotNamePrefix) {
		t.Errorf("name %q should start with %q", got, snapshotNamePrefix)
	}
	// Format is e.g. "nivenia-20260426T143022Z" — exactly one timestamp
	// component after the prefix, ending in Z.
	suffix := got[len(snapshotNamePrefix):]
	if len(suffix) != len("20060102T150405Z") {
		t.Errorf("timestamp suffix %q has wrong length: got %d, want %d", suffix, len(suffix), len("20060102T150405Z"))
	}
	if !strings.HasSuffix(suffix, "Z") {
		t.Errorf("timestamp %q should end with Z", suffix)
	}
}

func TestFreshSnapshotName_IsUniqueAcrossCalls(t *testing.T) {
	t.Setenv("NIVENIA_SNAPSHOT_NAME", "")
	first := freshSnapshotName()
	// Sleep just past one second so the timestamp must differ. The
	// snapshot layout has 1-second resolution, which is sufficient for
	// distinct freezes (humans can't freeze faster than that).
	time.Sleep(1100 * time.Millisecond)
	second := freshSnapshotName()
	if first == second {
		t.Errorf("two freshSnapshotName() calls produced identical names: %q", first)
	}
}

func TestFreshSnapshotName_EnvOverrideWins(t *testing.T) {
	t.Setenv("NIVENIA_SNAPSHOT_NAME", "custom-name")
	if got := freshSnapshotName(); got != "custom-name" {
		t.Errorf("env override: got %q, want custom-name", got)
	}
}

// ── SnapshotName ──────────────────────────────────────────────────────────────

func TestSnapshotName_FallsBackToEnvWhenStateMissing(t *testing.T) {
	// loadSnapshotState reads /var/lib/nivenia/snapshot.json. In tests
	// that path may exist on the host (if the dev has Nivenia installed),
	// so we can only assert the env-fallback path: when no state and an
	// env override is set, that override is what callers see.
	t.Setenv("NIVENIA_SNAPSHOT_NAME", "env-test-snapshot")
	if _, ok := loadSnapshotState(); ok {
		t.Skip("host has /var/lib/nivenia/snapshot.json — env-fallback can't be tested here")
	}
	if got := SnapshotName(); got != "env-test-snapshot" {
		t.Errorf("got %q, want env-test-snapshot", got)
	}
}
