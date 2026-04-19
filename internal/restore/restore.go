package restore

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func canonical(p string) string {
	cleaned := filepath.Clean(p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	return nil
}

func rsyncExcludeArgs(excludes []string) []string {
	args := make([]string, 0, len(excludes)*2)
	for _, p := range excludes {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		args = append(args, "--exclude", strings.TrimPrefix(canonical(trimmed), "/"))
	}
	return args
}

func runRsync(src, dst string, excludes []string, delete bool) error {
	args := []string{"-aH", "--numeric-ids"}
	if delete {
		args = append(args, "--delete")
	}
	args = append(args, rsyncExcludeArgs(excludes)...)
	args = append(args, src, dst)

	cmd := exec.Command("rsync", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func CaptureBaseline(managedRoot, baselineRoot string, excludes []string) error {
	src := canonical(managedRoot)
	dst := canonical(baselineRoot)

	if src == dst {
		return fmt.Errorf("managed_root and baseline_root must be different")
	}
	if err := ensureDir(dst); err != nil {
		return err
	}

	return runRsync(src+"/", dst+"/", excludes, true)
}

func RestoreFromBaseline(baselineRoot, managedRoot string, excludes []string) error {
	src := canonical(baselineRoot)
	dst := canonical(managedRoot)

	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("baseline missing at %s: %w", src, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("baseline path %s is not a directory", src)
	}

	return runRsync(src+"/", dst+"/", excludes, true)
}
