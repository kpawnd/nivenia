package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/engine"
	"nivenia/internal/platform"
)

func waitForBootReadiness() {
	// Keep this conservative: small fixed wait, then probe for loginwindow.
	time.Sleep(20 * time.Second)

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if exec.Command("pgrep", "-x", "loginwindow").Run() == nil {
			return
		}
		time.Sleep(5 * time.Second)
	}
}

func main() {
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}

	waitForBootReadiness()

	p, err := config.Load(*policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "niveniad: %v\n", err)
		os.Exit(1)
	}

	e := engine.New(p)
	if err := e.RunBootRestore(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad restore failed: %v\n", err)
		os.Exit(2)
	}
}
