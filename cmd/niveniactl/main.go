package main

import (
	"flag"
	"fmt"
	"os"

	"nivenia/internal/config"
	"nivenia/internal/platform"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

func usage() {
	fmt.Print(`Nivenia control CLI

Usage:
	niveniactl [--state PATH] [--policy PATH] <command>

Commands:
	status       Show current freeze mode and last restore status
	freeze       Capture a new baseline and set mode to frozen
	thaw         Set mode to thawed (no restore on boot)
	thaw-once    Skip restore for next boot only, then return to frozen
	help         Show this help

Options:
	--state PATH   State file path (default: /var/lib/nivenia/state.json)
	--policy PATH  Policy file path (default: /etc/nivenia/policy.json)

Examples:
	sudo niveniactl status
	sudo niveniactl freeze --policy /etc/nivenia/policy.json --state /var/lib/nivenia/state.json
	sudo niveniactl thaw
	sudo niveniactl thaw-once
`)
}

func main() {
	statePath := flag.String("state", "/var/lib/nivenia/state.json", "state file path")
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniactl: %v\n", err)
		os.Exit(1)
	}

	if flag.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	cmd := flag.Arg(0)
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		usage()
		return
	}

	s, err := state.Load(*statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load state: %v\n", err)
		os.Exit(2)
	}

	switch cmd {
	case "status":
		fmt.Printf("mode=%s last_restore_ok=%t message=%q updated=%s\n", s.Mode, s.LastRestoreOK, s.LastMessage, s.UpdatedAtUTC)
		return
	case "freeze":
		p, err := config.Load(*policyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load policy: %v\n", err)
			os.Exit(4)
		}
		if err := restore.CaptureBaseline(p.ManagedRoot, p.BaselineRoot, p.ExcludePaths); err != nil {
			fmt.Fprintf(os.Stderr, "capture baseline: %v\n", err)
			os.Exit(5)
		}
		s.Mode = state.ModeFrozen
		s.LastMessage = "mode set to frozen; baseline captured"
	case "thaw":
		s.Mode = state.ModeThawed
		s.LastMessage = "mode set to thawed"
	case "thaw-once":
		s.Mode = state.ModeThawOnce
		s.LastMessage = "mode set to thaw_once"
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}

	if err := state.Save(*statePath, s); err != nil {
		fmt.Fprintf(os.Stderr, "save state: %v\n", err)
		os.Exit(3)
	}
	fmt.Println("ok")
}
