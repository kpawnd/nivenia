package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nivenia/internal/restore"
)

const (
	integrityVersion     = 1
	defaultIntegrityPath = "/var/lib/nivenia/integrity.json"
)

type SnapshotMetadata struct {
	Name      string `json:"name"`
	XID       string `json:"xid"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Raw       string `json:"raw"`
}

type IntegrityRecord struct {
	Version        int               `json:"version"`
	CreatedAtUTC   string            `json:"created_at_utc"`
	ManagedRoot    string            `json:"managed_root"`
	SnapshotVolume string            `json:"snapshot_volume"`
	SnapshotName   string            `json:"snapshot_name"`
	Snapshot       SnapshotMetadata  `json:"snapshot"`
	VolumeUUID     string            `json:"volume_uuid"`
	PolicyPath     string            `json:"policy_path"`
	PolicyHash     string            `json:"policy_hash"`
	BinaryHashes   map[string]string `json:"binary_hashes"`
}

func IntegrityPath() string {
	if env := strings.TrimSpace(os.Getenv("NIVENIA_INTEGRITY_FILE")); env != "" {
		return env
	}
	return defaultIntegrityPath
}

func CaptureBaseline(policyPath, managedRoot string) error {
	volume := restore.SnapshotVolume(managedRoot)
	name := restore.SnapshotName()

	volUUID, err := volumeUUID(volume)
	if err != nil {
		return err
	}

	snap, err := snapshotMetadata(volume, name)
	if err != nil {
		return err
	}

	policyHash, err := fileHash(policyPath)
	if err != nil {
		return fmt.Errorf("policy hash: %w", err)
	}

	binHashes, err := hashBinarySet()
	if err != nil {
		return err
	}

	record := IntegrityRecord{
		Version:        integrityVersion,
		CreatedAtUTC:   time.Now().UTC().Format(time.RFC3339),
		ManagedRoot:    managedRoot,
		SnapshotVolume: volume,
		SnapshotName:   name,
		Snapshot:       snap,
		VolumeUUID:     volUUID,
		PolicyPath:     policyPath,
		PolicyHash:     policyHash,
		BinaryHashes:   binHashes,
	}

	return writeRecord(record)
}

// VerifySnapshotOnly confirms the snapshot we're about to restore from is
// the same APFS snapshot that was captured at freeze time. It checks the
// managed root, snapshot volume, snapshot name, the volume's UUID, and the
// snapshot's XID / UUID / Timestamp.
//
// Unlike VerifyBaseline, this function deliberately does NOT hash binaries
// or the policy file — those legitimately change between freeze and the
// next boot whenever the updater runs, and distinguishing legitimate updates
// from tampering would require a signing scheme that is out of scope here.
// Snapshot metadata, by contrast, cannot change without someone swapping the
// snapshot out from under us — exactly the threat that would cause rsync
// --delete to wipe the wrong files.
//
// This is the check invoked on every boot before a restore runs, so it must
// be cheap (no rsync, no hashing) and fail closed on any mismatch.
func VerifySnapshotOnly(managedRoot string) error {
	path := IntegrityPath()
	record, err := readRecord(path)
	if err != nil {
		return err
	}
	if record.Version != integrityVersion {
		return fmt.Errorf("integrity version mismatch: %d", record.Version)
	}
	if record.ManagedRoot != managedRoot {
		return fmt.Errorf("managed_root mismatch: expected %s got %s", record.ManagedRoot, managedRoot)
	}

	volume := restore.SnapshotVolume(managedRoot)
	name := restore.SnapshotName()
	if record.SnapshotVolume != volume {
		return fmt.Errorf("snapshot volume mismatch: expected %s got %s", record.SnapshotVolume, volume)
	}
	if record.SnapshotName != name {
		return fmt.Errorf("snapshot name mismatch: expected %s got %s", record.SnapshotName, name)
	}

	volUUID, err := volumeUUID(volume)
	if err != nil {
		return err
	}
	if record.VolumeUUID != "" && volUUID != record.VolumeUUID {
		return fmt.Errorf("volume uuid mismatch: expected %s got %s", record.VolumeUUID, volUUID)
	}

	snap, err := snapshotMetadata(volume, name)
	if err != nil {
		return err
	}
	if record.Snapshot.XID != "" && snap.XID != record.Snapshot.XID {
		return fmt.Errorf("snapshot XID mismatch: expected %s got %s", record.Snapshot.XID, snap.XID)
	}
	if record.Snapshot.UUID != "" && snap.UUID != record.Snapshot.UUID {
		return fmt.Errorf("snapshot UUID mismatch: expected %s got %s", record.Snapshot.UUID, snap.UUID)
	}
	if record.Snapshot.Timestamp != "" && snap.Timestamp != record.Snapshot.Timestamp {
		return fmt.Errorf("snapshot timestamp mismatch: expected %s got %s", record.Snapshot.Timestamp, snap.Timestamp)
	}
	return nil
}

func VerifyBaseline(policyPath, managedRoot string) error {
	path := IntegrityPath()
	record, err := readRecord(path)
	if err != nil {
		return err
	}
	if record.Version != integrityVersion {
		return fmt.Errorf("integrity version mismatch: %d", record.Version)
	}
	if record.ManagedRoot != managedRoot {
		return fmt.Errorf("managed_root mismatch: expected %s got %s", record.ManagedRoot, managedRoot)
	}

	volume := restore.SnapshotVolume(managedRoot)
	name := restore.SnapshotName()
	if record.SnapshotVolume != volume {
		return fmt.Errorf("snapshot volume mismatch: expected %s got %s", record.SnapshotVolume, volume)
	}
	if record.SnapshotName != name {
		return fmt.Errorf("snapshot name mismatch: expected %s got %s", record.SnapshotName, name)
	}

	volUUID, err := volumeUUID(volume)
	if err != nil {
		return err
	}
	if record.VolumeUUID != "" && volUUID != record.VolumeUUID {
		return fmt.Errorf("volume uuid mismatch: expected %s got %s", record.VolumeUUID, volUUID)
	}

	snap, err := snapshotMetadata(volume, name)
	if err != nil {
		return err
	}
	if record.Snapshot.XID != "" && snap.XID != record.Snapshot.XID {
		return fmt.Errorf("snapshot XID mismatch: expected %s got %s", record.Snapshot.XID, snap.XID)
	}
	if record.Snapshot.UUID != "" && snap.UUID != record.Snapshot.UUID {
		return fmt.Errorf("snapshot UUID mismatch: expected %s got %s", record.Snapshot.UUID, snap.UUID)
	}
	if record.Snapshot.Timestamp != "" && snap.Timestamp != record.Snapshot.Timestamp {
		return fmt.Errorf("snapshot timestamp mismatch: expected %s got %s", record.Snapshot.Timestamp, snap.Timestamp)
	}

	policyHash, err := fileHash(policyPath)
	if err != nil {
		return fmt.Errorf("policy hash: %w", err)
	}
	if record.PolicyHash != "" && policyHash != record.PolicyHash {
		return fmt.Errorf("policy hash mismatch")
	}

	if err := verifyBinarySet(record.BinaryHashes); err != nil {
		return err
	}

	return nil
}

func writeRecord(record IntegrityRecord) error {
	path := IntegrityPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	// fsync before rename so a power loss doesn't leave a zero-byte integrity
	// file; fsync the directory so the rename itself is durable.
	tmp := path + ".tmp"
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
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func readRecord(path string) (IntegrityRecord, error) {
	var record IntegrityRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return record, fmt.Errorf("read integrity file: %w", err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("parse integrity file: %w", err)
	}
	return record, nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func defaultBinaryPaths() []string {
	return []string{
		"/usr/local/libexec/niveniad",
		"/usr/local/bin/niveniactl",
		"/usr/local/libexec/nivenia-updater",
		"/usr/local/bin/nivenia-update",
		"/usr/local/bin/nivenia-recovery",
		"/var/lib/nivenia/recovery/nivenia-recovery.sh",
		"/usr/local/bin/nivenia-prepare-clean-capture",
		"/Library/LaunchDaemons/com.nivenia.restore.plist",
		"/Library/LaunchDaemons/com.nivenia.updater.plist",
	}
}

func hashBinarySet() (map[string]string, error) {
	paths := defaultBinaryPaths()
	hashes := make(map[string]string, len(paths))
	for _, path := range paths {
		sum, err := fileHash(path)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", path, err)
		}
		hashes[path] = sum
	}
	return hashes, nil
}

func verifyBinarySet(expected map[string]string) error {
	for path, want := range expected {
		sum, err := fileHash(path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", path, err)
		}
		if want != "" && sum != want {
			return fmt.Errorf("binary hash mismatch: %s", path)
		}
	}
	return nil
}

func volumeUUID(volume string) (string, error) {
	out, err := runDiskutil("info", volume)
	if err != nil {
		return "", err
	}
	uuid := parseInfoValue(out, "Volume UUID:")
	if uuid == "" {
		uuid = parseInfoValue(out, "APFS Volume UUID:")
	}
	if uuid == "" {
		return "", fmt.Errorf("volume UUID not found for %s", volume)
	}
	return uuid, nil
}

func snapshotMetadata(volume, name string) (SnapshotMetadata, error) {
	out, err := runDiskutil("apfs", "listSnapshots", volume)
	if err != nil {
		return SnapshotMetadata{}, err
	}
	lines := strings.Split(out, "\n")
	var current []string
	var currentName string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Snapshot Name:") {
			if currentName == name {
				break
			}
			currentName = strings.TrimSpace(strings.TrimPrefix(trimmed, "Snapshot Name:"))
			current = []string{trimmed}
			continue
		}
		if strings.HasPrefix(trimmed, "Name:") {
			if currentName == name {
				break
			}
			currentName = strings.TrimSpace(strings.TrimPrefix(trimmed, "Name:"))
			current = []string{trimmed}
			continue
		}
		if currentName != "" {
			if trimmed == "" {
				current = append(current, trimmed)
				continue
			}
			current = append(current, trimmed)
		}
	}
	if currentName != name {
		return SnapshotMetadata{}, fmt.Errorf("snapshot %q not found on %s", name, volume)
	}

	meta := SnapshotMetadata{Name: name}
	for _, line := range current {
		switch {
		case strings.HasPrefix(line, "XID:"):
			meta.XID = strings.TrimSpace(strings.TrimPrefix(line, "XID:"))
		case strings.HasPrefix(line, "Snapshot UUID:"):
			meta.UUID = strings.TrimSpace(strings.TrimPrefix(line, "Snapshot UUID:"))
		case strings.HasPrefix(line, "UUID:") && meta.UUID == "":
			meta.UUID = strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))
		case strings.HasPrefix(line, "Timestamp:"):
			meta.Timestamp = strings.TrimSpace(strings.TrimPrefix(line, "Timestamp:"))
		}
	}
	meta.Raw = strings.Join(current, "\n")

	return meta, nil
}

func runDiskutil(args ...string) (string, error) {
	cmd := exec.Command("diskutil", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("diskutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func parseInfoValue(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		}
	}
	return ""
}
