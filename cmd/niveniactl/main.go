// niveniactl — admin CLI for Nivenia.
//
// Subcommands:
//
//	status     show mode + last restore outcome (pretty if TTY)
//	freeze     capture a fresh APFS snapshot as the new baseline
//	thaw       set thawed mode — restore is skipped on every boot
//	thaw-once  skip exactly one boot's restore, then return to frozen
//	verify     run the full integrity check (snapshot + binaries + policy)
//	logs       print a recent slice of the structured restore log
//	help       show usage
//
// `freeze` is the only state-mutating command that touches APFS. The
// implementation lives in internal/restore — see that package's
// header comment for why we use tmutil rather than diskutil for
// snapshot creation on Sequoia.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"nivenia/internal/config"
	"nivenia/internal/integrity"
	"nivenia/internal/nivlog"
	"nivenia/internal/platform"
	"nivenia/internal/restore"
	"nivenia/internal/state"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func printStatusPretty(s state.State, snap, osVer string) {
	rule := ansiCyan + "  " + strings.Repeat("━", 60) + ansiReset

	var modeColor, modeSymbol string
	switch s.Mode {
	case state.ModeFrozen:
		modeColor = ansiCyan + ansiBold
		modeSymbol = "●"
	case state.ModeThawed:
		modeColor = ansiYellow + ansiBold
		modeSymbol = "○"
	case state.ModeThawOnce:
		modeColor = ansiYellow
		modeSymbol = "◑"
	default:
		modeColor = ansiReset
		modeSymbol = "·"
	}
	modeStr := strings.ToUpper(string(s.Mode))

	var restoreColor, restoreSymbol string
	if s.LastRestoreOK {
		restoreColor = ansiGreen + ansiBold
		restoreSymbol = "✓"
	} else {
		restoreColor = ansiRed + ansiBold
		restoreSymbol = "✗"
	}

	updated := s.UpdatedAtUTC
	if t, err := time.Parse(time.RFC3339, s.UpdatedAtUTC); err == nil {
		updated = t.UTC().Format("Mon 02 Jan 2006, 15:04 UTC")
	}

	msg := s.LastMessage
	if len(msg) > 80 {
		msg = msg[:79] + "…"
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("   %-10s%s%s %s%s\n", "Mode", modeColor, modeSymbol, modeStr, ansiReset)
	fmt.Printf("   %-10s%s%s%s  %s\n", "Restore", restoreColor, restoreSymbol, ansiReset, msg)
	fmt.Printf("   %s%-10s%s%s\n", ansiDim, "Updated", updated, ansiReset)
	if snap != "" {
		fmt.Printf("   %s%-10s%s%s\n", ansiDim, "Snapshot", snap, ansiReset)
	}
	if osVer != "" {
		fmt.Printf("   %s%-10s%s on macOS %s%s\n", ansiDim, "Version", version, osVer, ansiReset)
	}
	fmt.Printf("   %s%-10s%s%d%s\n", ansiDim, "Failures", "", s.FailureCount, ansiReset)
	fmt.Println(rule)
	fmt.Println()
}

func usage() {
	fmt.Print(`Nivenia control CLI

Usage:
    niveniactl [--state PATH] [--policy PATH] <command>

Commands:
    status     Show current freeze mode and last restore status
    freeze     Capture a new baseline and set mode to frozen
    thaw       Set mode to thawed (no restore on boot)
    thaw-once  Skip restore for next boot only, then return to frozen
    verify     Run the full integrity check on baseline + binaries
    logs       Tail the structured restore log
    help       Show this help

Options:
    --state PATH   State file path (default: /var/lib/nivenia/state.json)
    --policy PATH  Policy file path (default: /etc/nivenia/policy.json)
    --version      Print version and exit
`)
}

// tailLines streams the last N non-empty lines from path to stdout.
// We intentionally don't use the Go-only generic "tail" because the
// log file can be large; the implementation is a chunked reverse-read.
func tailLines(path string, n int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	const chunk = 16 * 1024
	size := st.Size()
	if size == 0 {
		return nil
	}
	var lines []string
	var buf []byte
	pos := size
	for pos > 0 && len(lines) < n+1 {
		readLen := int64(chunk)
		if pos < readLen {
			readLen = pos
		}
		pos -= readLen
		tmp := make([]byte, readLen)
		if _, err := f.ReadAt(tmp, pos); err != nil && err != io.EOF {
			return err
		}
		buf = append(tmp, buf...)
		lines = strings.Split(string(buf), "\n")
		// Re-buffer the leading partial line (might split mid-line).
		if pos > 0 {
			buf = []byte(lines[0])
			lines = lines[1:]
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		fmt.Println(l)
	}
	return nil
}

func main() {
	statePath := flag.String("state", "/var/lib/nivenia/state.json", "state file path")
	policyPath := flag.String("policy", "/etc/nivenia/policy.json", "policy file path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if err := platform.EnsureSupportedMacOS(); err != nil {
		fmt.Fprintf(os.Stderr, "niveniactl: %v\n", err)
		os.Exit(1)
	}
	if *showVersion {
		fmt.Println(version)
		return
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
		osVer, _ := platform.MacOSProductVersion()
		snap := restore.SnapshotName()
		if isTTY() {
			printStatusPretty(s, snap, osVer)
		} else {
			fmt.Printf("mode=%s last_restore_ok=%t failures=%d snapshot=%q os=%s nivenia=%s message=%q updated=%s\n",
				s.Mode, s.LastRestoreOK, s.FailureCount, snap, osVer, version, s.LastMessage, s.UpdatedAtUTC)
		}
		return
	case "verify":
		// Full integrity check: snapshot + policy hash + binary
		// hashes. Useful as a post-update sanity pass.
		p, err := config.Load(*policyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load policy: %v\n", err)
			os.Exit(4)
		}
		if err := integrity.VerifyBaseline(p.PolicyPath, p.ManagedRoot); err != nil {
			fmt.Fprintf(os.Stderr, "verify failed: %v\n", err)
			os.Exit(2)
		}
		fmt.Println("ok: snapshot, policy, binaries all match the captured baseline")
		return
	case "logs":
		// Print last ~50 lines of the structured log so admins can
		// triage without learning where the log file lives.
		p, err := config.Load(*policyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load policy: %v\n", err)
			os.Exit(4)
		}
		if err := tailLines(p.LogFile, 50); err != nil {
			fmt.Fprintf(os.Stderr, "read log %s: %v\n", p.LogFile, err)
			os.Exit(2)
		}
		return
	case "freeze":
		p, err := config.Load(*policyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load policy: %v\n", err)
			os.Exit(4)
		}
		osVer, _ := platform.MacOSProductVersion()
		log := nivlog.New(p.LogFile, "/var/log/nivenia", "niveniactl", version, osVer)
		log.Info("freeze.invoked", "os", osVer, "policy_path", p.PolicyPath)

		if err := restore.CaptureBaseline(log, p.ManagedRoot); err != nil {
			log.Error("freeze.fail", "error", err.Error())
			fmt.Fprintf(os.Stderr, "capture baseline: %v\n", err)
			os.Exit(5)
		}
		if err := integrity.CaptureBaseline(p.PolicyPath, p.ManagedRoot); err != nil {
			log.Error("integrity.capture.fail", "error", err.Error())
			fmt.Fprintf(os.Stderr, "integrity capture: %v\n", err)
			os.Exit(6)
		}
		s.Mode = state.ModeFrozen
		s.LastRestoreOK = true
		s.FailureCount = 0
		s.LastMessage = "mode set to frozen; baseline and integrity captured"
		log.Info("freeze.done", "snapshot", restore.SnapshotName())
	case "thaw":
		s.Mode = state.ModeThawed
		s.LastRestoreOK = true
		s.FailureCount = 0
		s.LastMessage = "mode set to thawed"
	case "thaw-once":
		s.Mode = state.ModeThawOnce
		s.LastRestoreOK = true
		s.FailureCount = 0
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
