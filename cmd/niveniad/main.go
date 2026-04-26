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
	"nivenia/internal/platform"
)

// version is stamped at build time via -ldflags "-X main.version=...".
// The default "dev" is what a plain `go build` produces.
var version = "dev"

func waitForManagedVolume(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("managed_root is empty")
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			// os.Stat can return nil before diskutil has fully registered the
			// volume with the APFS driver, triggering error -69854 on the next
			// diskutil call. Verify diskutil can reach the volume too.
			out, err := exec.Command("diskutil", "info", path).CombinedOutput()
			if err == nil && isAPFSVolume(string(out)) {
				return nil
			}
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

// probeFullDiskAccess does a best-effort detection of whether this
// LaunchDaemon has been granted Full Disk Access. On macOS Sonoma+ (and
// strictly enforced on Sequoia 15+), TCC gates root reads of certain
// user-library directories — even processes running as uid 0 get
// EPERM/EACCES if the binary's bundle isn't on the FDA list.
//
// We probe by attempting to enumerate ~user/Library/Calendars for the
// first real user we find. Calendars is created automatically the first
// time a user signs in to iCal/Calendar.app, so it's reliably present
// on lab Macs that have ever been used. If the directory exists and
// listing it yields a permission error while running as root, that is
// an unambiguous TCC denial.
//
// We DO NOT abort boot on a positive denial signal. The actual restore
// will still attempt rsync, and if FDA is required for the restore
// paths it will fail with a clear rsync error. The probe just emits a
// warning to stderr (captured by launchd into niveniad.err.log) so the
// admin sees the cause when triaging.
func probeFullDiskAccess() {
	// We need a real user — skip system slots and the public Shared
	// folder. /Users entries are normally directories owned by the
	// user (UID >= 500); we're not parsing /etc/passwd here, just
	// looking for a likely-real account directory.
	users, err := os.ReadDir("/Users")
	if err != nil {
		// Without /Users we can't probe; staying silent is correct
		// (the daemon may be running on a freshly imaged box where
		// no users have been created yet — restore would have
		// nothing to do anyway).
		return
	}
	for _, u := range users {
		name := u.Name()
		if !u.IsDir() || name == "Shared" || name == "Guest" || strings.HasPrefix(name, ".") {
			continue
		}
		// Calendars is one TCC-protected library subdirectory; Mail
		// would also work but isn't created until a user adds an
		// account. Calendars exists from first login.
		probe := filepath.Join("/Users", name, "Library", "Calendars")
		if _, statErr := os.Stat(probe); statErr != nil {
			// Path doesn't exist for this user; try the next.
			continue
		}
		_, readErr := os.ReadDir(probe)
		if readErr == nil {
			// Read succeeded — FDA is granted (or TCC isn't
			// gating this path). Either way, we're good.
			return
		}
		if errors.Is(readErr, os.ErrPermission) || strings.Contains(readErr.Error(), "operation not permitted") {
			fmt.Fprintf(os.Stderr,
				"[FDA] WARNING: cannot read %s as root (permission denied). "+
					"Likely cause: niveniad lacks Full Disk Access. "+
					"Grant via System Settings → Privacy & Security → Full Disk Access (add /usr/local/libexec/niveniad), "+
					"or push a PPPC profile via MDM. Boot restore may fail or skip protected user files.\n", probe)
			return
		}
		// Some other error (I/O, ENOENT race) — try the next user.
	}
}

func main() {
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// --version is the smoke test the updater runs on a freshly-staged
	// binary before it replaces the live one. It proves the executable is
	// linkable and the right architecture for this OS. We keep the platform
	// check in place so a new binary that doesn't support the running macOS
	// version fails the smoke test instead of being installed.
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

	if err := waitForManagedVolume(p.ManagedRoot); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad preboot check failed: %v\n", err)
		os.Exit(1)
	}

	// Best-effort FDA detection — emits a stderr warning if it looks
	// like Full Disk Access is missing. Never blocks boot; the actual
	// rsync will produce its own errors if FDA is genuinely required
	// for the restore paths in this policy.
	probeFullDiskAccess()

	// Cancel the context on SIGTERM or SIGINT so the rsync child is killed
	// before we exit, preventing orphaned rsyncs on daemon stop or reboot.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	e := engine.New(p)
	if err := e.RunBootRestore(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad restore failed: %v\n", err)
		os.Exit(2)
	}
}
