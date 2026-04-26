// Package platform contains the macOS-version gate. Nivenia targets
// Sequoia (macOS 15) only — every other macOS version (Sonoma, Ventura,
// Monterey, and earlier) is rejected at startup so we never ship a build
// that silently misbehaves on an environment we can't test against.
//
// The reason for the tight bound: Sequoia ships openrsync (a clean-room
// reimplementation), uses a specific TCC story for LaunchDaemons, and
// has the snapshot-creation surface we depend on (`tmutil localsnapshot`
// + `mount_apfs -s`). Older versions ship Apple's classic rsync 2.6.9
// with different bugs and flags, different TCC behaviour, and slightly
// different diskutil verbs. We were chasing those differences and that
// is what caused the boot-restore regressions visible in the logs.
package platform

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const (
	// Sequoia 15 is the only supported macOS major. Tahoe (16) will be
	// added once it is available for testing; until then, refuse it
	// rather than risk silently broken behaviour on an unverified OS.
	SupportedMajor = 15
)

func majorVersion(version string) (int, error) {
	trimmed := strings.TrimSpace(version)
	if trimmed == "" {
		return 0, fmt.Errorf("empty version string")
	}
	parts := strings.Split(trimmed, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid version %q: %w", trimmed, err)
	}
	return major, nil
}

func MacOSProductVersion() (string, error) {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "", fmt.Errorf("read macOS version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureSupportedMacOS returns nil only on macOS Sequoia (15). The
// daemon, CLI, and updater all gate on this so an unsupported OS gets
// a clear error at startup instead of a confusing rsync/diskutil
// failure several minutes later.
func EnsureSupportedMacOS() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported OS %s: Nivenia targets macOS Sequoia (15) only", runtime.GOOS)
	}

	version, err := MacOSProductVersion()
	if err != nil {
		return err
	}
	major, err := majorVersion(version)
	if err != nil {
		return err
	}
	if major != SupportedMajor {
		return fmt.Errorf("unsupported macOS %s: Nivenia targets macOS Sequoia (15) only", version)
	}
	return nil
}
