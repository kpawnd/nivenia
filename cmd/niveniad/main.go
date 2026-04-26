// niveniad — boot-time restore daemon.
//
// Lifecycle:
//  1. launchd starts us at boot via com.nivenia.restore.plist.
//  2. We confirm the OS is Sequoia and load the policy.
//  3. We wait for the managed Data volume to come online (APFS can
//     report partial readiness for several seconds during fast boots).
//  4. We probe Full Disk Access (warns only — never blocks).
//  5. We hand control to engine.RunBootRestore which acquires the
//     lock, verifies the snapshot, mounts it, and rsyncs.
//
// Every step writes structured log events to /var/log/nivenia.log (and
// the per-run rsync transcripts go to /var/log/nivenia/rsync-*.log)
// so post-mortem triage doesn't depend on stitching together stderr.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/engine"
	"nivenia/internal/nivlog"
	"nivenia/internal/platform"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// The default "dev" is what a plain `go build` produces.
var version = "dev"

// volumeWaitDeadline is how long we wait for the Data volume to be
// mounted and APFS-registered before giving up. 10 minutes is generous
// enough that fsck + APFS housekeeping after a long-idle boot can
// finish, while still bounded so a genuinely-broken disk doesn't stall
// indefinitely. NIVENIA_VOLUME_WAIT_SECONDS overrides for slower hardware.
func volumeWaitDeadline() time.Duration {
	if v := os.Getenv("NIVENIA_VOLUME_WAIT_SECONDS"); v != "" {
		if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
			return secs
		}
	}
	return 10 * time.Minute
}

func waitForManagedVolume(log *nivlog.Logger, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("managed_root is empty")
	}
	deadline := time.Now().Add(volumeWaitDeadline())
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		if _, err := os.Stat(path); err == nil {
			out, dErr := exec.Command("diskutil", "info", path).CombinedOutput()
			if dErr == nil && isAPFSVolume(string(out)) {
				if attempt > 1 {
					log.Info("volume.ready", "path", path, "attempts", attempt)
				}
				return nil
			}
		}
		if attempt%30 == 0 {
			// Every minute, log that we're still waiting so the
			// transcript shows steady progress instead of a silent
			// hang.
			log.Info("volume.waiting", "path", path, "elapsed", time.Since(time.Now().Add(-time.Duration(attempt*2)*time.Second)).Round(time.Second).String())
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for managed_root: %s", path)
}

func isAPFSVolume(info string) bool {
	upper := strings.ToUpper(info)
	return strings.Contains(upper, "FILE SYSTEM PERSONALITY: APFS") ||
		strings.Contains(upper, "TYPE (BUNDLE): APFS") ||
		strings.Contains(upper, "APFS VOLUME")
}

// probeFullDiskAccess: best-effort detection of TCC blocking root
// reads of user library subdirectories. Logs a warning but never
// blocks boot — many lab Macs don't have any TCC-gated content.
func probeFullDiskAccess(log *nivlog.Logger) {
	users, err := os.ReadDir("/Users")
	if err != nil {
		return
	}
	for _, u := range users {
		name := u.Name()
		if !u.IsDir() || name == "Shared" || name == "Guest" || strings.HasPrefix(name, ".") {
			continue
		}
		probe := filepath.Join("/Users", name, "Library", "Calendars")
		if _, statErr := os.Stat(probe); statErr != nil {
			continue
		}
		if _, readErr := os.ReadDir(probe); readErr == nil {
			log.Info("fda.probe.ok", "path", probe)
			return
		} else if errors.Is(readErr, os.ErrPermission) || strings.Contains(readErr.Error(), "operation not permitted") {
			log.Warn("fda.missing",
				"path", probe,
				"hint", "grant Full Disk Access to /usr/local/libexec/niveniad in System Settings → Privacy & Security, or push a PPPC profile via MDM",
			)
			return
		}
	}
}

func main() {
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}
	if *showVersion {
		fmt.Println(version)
		return
	}

	p, err := config.Load(*policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}

	osVer, _ := platform.MacOSProductVersion()
	logger := nivlog.New(p.LogFile, "/var/log/nivenia", "niveniad", version, osVer)
	logger.Info("boot.invoked",
		"os", osVer,
		"policy_path", *policyPath,
	)

	if err := waitForManagedVolume(logger, p.ManagedRoot); err != nil {
		logger.Error("volume.timeout", "path", p.ManagedRoot, "error", err.Error())
		os.Exit(1)
	}

	probeFullDiskAccess(logger)

	// Cancel the context on SIGTERM/SIGINT so the rsync child is
	// killed before we exit, preventing orphaned rsyncs.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	e := engine.New(p)
	e.Log = logger
	if err := e.RunBootRestore(ctx); err != nil {
		logger.Error("boot.fail", "error", err.Error())
		os.Exit(2)
	}
}
