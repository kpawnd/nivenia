package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Policy struct {
	ManagedRoot  string   `json:"managed_root"`
	RestorePaths []string `json:"restore_paths"`
	StateFile    string   `json:"state_file"`
	LogFile      string   `json:"log_file"`
	PolicyPath   string   `json:"-"`
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
	p.PolicyPath = path
	if p.ManagedRoot == "" {
		p.ManagedRoot = "/System/Volumes/Data"
	}
	if len(p.RestorePaths) == 0 {
		p.RestorePaths = []string{
			filepath.Join(p.ManagedRoot, "Users"),
			filepath.Join(p.ManagedRoot, "Applications"),
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
