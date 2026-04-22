package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/engine"
	"nivenia/internal/platform"
)

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

func main() {
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
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
