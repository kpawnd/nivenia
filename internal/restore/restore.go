package restore

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

// SpeedFormatter converts bytes to human-readable speed
func formatSpeed(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.2f GB/s", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.2f MB/s", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.2f KB/s", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B/s", bytes)
	}
}

func runRsync(src, dst string, excludes []string, delete bool) error {
	args := []string{"-aH", "--numeric-ids"}
	if delete {
		args = append(args, "--delete")
	}
	// Optimizations for initial capture:
	// --whole-file: disable delta algorithm (faster for local copies)
	// --progress: show progress (compatible with older rsync versions like macOS 2.6.9)
	args = append(args, "--whole-file", "--progress")
	args = append(args, rsyncExcludeArgs(excludes)...)
	args = append(args, src, dst)

	cmd := exec.Command("rsync", args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("rsync start: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	spinner := []string{"|", "/", "-", "\\"}
	spinIdx := 0
	speedRegex := regexp.MustCompile(`(\d+\.\d+)([KMG])B/s`)
	lastSpeed := ""
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var errorLines []string

	go func() {
		for range ticker.C {
			fmt.Fprintf(os.Stderr, "\r\033[2K[CAP] Capturing baseline %s %s", spinner[spinIdx%len(spinner)], lastSpeed)
			spinIdx++
		}
	}()

	for scanner.Scan() {
		line := scanner.Text()
		if matches := speedRegex.FindStringSubmatch(line); len(matches) > 0 {
			lastSpeed = matches[1] + matches[2] + "B/s"
		} else if strings.TrimSpace(line) != "" && !strings.Contains(line, "to-check") && !strings.Contains(line, "sent") && !strings.Contains(line, "total") {
			// Capture non-progress error lines
			errorLines = append(errorLines, line)
		}
	}

	ticker.Stop()
	fmt.Fprintf(os.Stderr, "\r\033[2K")

	if err := cmd.Wait(); err != nil {
		// Build detailed error message with rsync arguments for debugging
		errMsg := fmt.Sprintf("rsync failed: %v\ncommand: rsync %s", err, strings.Join(args, " "))
		if len(errorLines) > 0 {
			errMsg += "\nrsync output:\n" + strings.Join(errorLines, "\n")
		}
		return fmt.Errorf(errMsg)
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
