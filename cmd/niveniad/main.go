package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
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
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for managed_root: %s", path)
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

	e := engine.New(p)
	if err := e.RunBootRestore(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad restore failed: %v\n", err)
		os.Exit(2)
	}
}
