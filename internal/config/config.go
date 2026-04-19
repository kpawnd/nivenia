package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type Policy struct {
	ManagedRoot  string   `json:"managed_root"`
	BaselineRoot string   `json:"baseline_root"`
	ExcludePaths []string `json:"exclude_paths"`
	StateFile    string   `json:"state_file"`
	LogFile      string   `json:"log_file"`
}

func Load(path string) (Policy, error) {
	var p Policy
	b, err := os.ReadFile(path)
	if err != nil {
		return p, fmt.Errorf("read policy: %w", err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("parse policy: %w", err)
	}
	if p.BaselineRoot == "" {
		p.BaselineRoot = "/var/lib/nivenia/baseline"
	}
	if p.ManagedRoot == "" {
		p.ManagedRoot = "/System/Volumes/Data"
	}
	if len(p.ExcludePaths) == 0 {
		p.ExcludePaths = []string{
			"/private/tmp",
			"/private/var/tmp",
			"/private/var/run",
			"/private/var/vm",
			"/private/var/folders",
			"/private/var/log",
			"/private/var/db/nivenia",
			"/private/var/lib/nivenia",
			"/private/var/db/diagnostics",
			"/private/var/db/DetachedSignatures",
			"/private/var/db/AuthenticationAuthority",
			"/private/var/db/KerberosKDC",
			"/private/var/protected",
			"/private/etc/fstab",
			"/Library/Caches",
			"/Library/Frameworks",
			"/Library/SystemExtensions",
			"/Library/Logs",
			"/Volumes",
			"/dev",
		}
	}
	if p.StateFile == "" {
		p.StateFile = "/var/lib/nivenia/state.json"
	}
	if p.LogFile == "" {
		p.LogFile = "/var/log/nivenia.log"
	}
	return p, nil
}
