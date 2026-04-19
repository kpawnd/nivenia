package main

import (
	"flag"
	"fmt"
	"os"

	"nivenia/internal/config"
	"nivenia/internal/engine"
	"nivenia/internal/platform"
)

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

	e := engine.New(p)
	if err := e.RunBootRestore(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniad restore failed: %v\n", err)
		os.Exit(2)
	}
}
