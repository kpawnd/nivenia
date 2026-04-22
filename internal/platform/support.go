package platform

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const (
	MinSupportedMajor = 12 // Monterey
	MaxSupportedMajor = 15 // Sequoia
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

func EnsureSupportedMacOS() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported OS %s: only macOS Monterey through Sequoia is supported", runtime.GOOS)
	}

	version, err := MacOSProductVersion()
	if err != nil {
		return err
	}
	major, err := majorVersion(version)
	if err != nil {
		return err
	}
	if major < MinSupportedMajor || major > MaxSupportedMajor {
		return fmt.Errorf("unsupported macOS %s: supported versions are Monterey (12) through Sequoia (15)", version)
	}
	return nil
}
