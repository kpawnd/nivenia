package config

import (
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"strings"
)

type Policy struct {
	ManagedRoot  string   `json:"managed_root"`
	RestorePaths []string `json:"restore_paths"`
	StateFile    string   `json:"state_file"`
	LogFile      string   `json:"log_file"`
	PolicyPath   string   `json:"-"`
}

// equivalentPaths returns the set of paths that, on macOS Big Sur+,
// resolve to the same set of files as p. /var, /Users, /Applications
// and friends are firmlinks from the read-only System volume to
// /System/Volumes/Data/var etc., so rsync into either form touches
// the same files. We list both forms so the guard check below catches
// a restore_path written either way.
//
// We deliberately do NOT use this for the managed-root containment
// check. There, "/some/other/volume/Users" should be rejected even
// though we could mechanically prefix it onto /System/Volumes/Data —
// the admin's intent matters and a non-Data volume is not equivalent.
//
// We use the path package (POSIX) rather than path/filepath because
// these strings come from policy.json and target macOS regardless of
// where the binary is built or tested.
func equivalentPaths(p string) []string {
	const dataPrefix = "/System/Volumes/Data"
	clean := pathpkg.Clean(p)
	forms := []string{clean}
	switch {
	case clean == dataPrefix:
		forms = append(forms, "/")
	case strings.HasPrefix(clean, dataPrefix+"/"):
		forms = append(forms, clean[len(dataPrefix):])
	case strings.HasPrefix(clean, "/"):
		forms = append(forms, dataPrefix+clean)
	}
	return forms
}

// literalContains reports whether child is the same path as parent or
// sits inside parent's directory tree using literal POSIX prefix
// comparison (no firmlink interpretation). Used for the managed-root
// check, where the policy must literally place restore paths inside
// the configured root.
func literalContains(parent, child string) bool {
	p := pathpkg.Clean(parent)
	c := pathpkg.Clean(child)
	if c == p {
		return true
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return strings.HasPrefix(c, p)
}

// guardCovered reports whether a guard path lives inside any of the
// firmlink-equivalent forms of restorePath. Used to detect policies
// that would silently rsync over /var/lib/nivenia/state.json (or any
// other Nivenia-owned file) on every boot.
func guardCovered(guard, restorePath string) bool {
	for _, rpForm := range equivalentPaths(restorePath) {
		for _, gForm := range equivalentPaths(guard) {
			if literalContains(rpForm, gForm) {
				return true
			}
		}
	}
	return false
}

// validate refuses any policy whose restore_paths would, on the next
// boot, rsync over Nivenia's own state files. Until this check existed,
// an admin tightening the policy by setting restore_paths to a parent
// like "/var" would silently restore /var/lib/nivenia/state.json from
// the snapshot every boot, freezing the control plane and making
// `niveniactl thaw` impossible to persist.
//
// Paths checked: state_file, log_file, snapshot.json, integrity.json,
// version marker, and the restore lock. Any of these falling inside a
// restore_path is a fatal config error — surfaced at startup with the
// specific conflict so the admin can fix the policy. We also enforce
// that every restore_path lives literally under managed_root.
func (p *Policy) validate() error {
	type protected struct {
		role string
		path string
	}
	guards := []protected{
		{"state_file", p.StateFile},
		{"log_file", p.LogFile},
		// These are constants in the restore/engine packages but
		// importing them would create a cycle; they're stable paths
		// (have not changed in the lifetime of the project) so
		// duplicating them as guards is acceptable.
		{"snapshot.json", "/var/lib/nivenia/snapshot.json"},
		{"integrity.json", "/var/lib/nivenia/integrity.json"},
		{"version marker", "/var/lib/nivenia/version"},
		{"restore lock", "/var/lib/nivenia/restore.lock"},
	}
	for _, rp := range p.RestorePaths {
		for _, g := range guards {
			if g.path == "" {
				continue
			}
			if guardCovered(g.path, rp) {
				return fmt.Errorf("policy invalid: restore_path %q would overwrite %s (%s) on every boot", rp, g.role, g.path)
			}
		}
	}
	// Every restore path must live literally under the managed root.
	// Firmlink equivalence does not apply: a policy that mixes
	// /System/Volumes/Data with /Volumes/External is almost certainly
	// a typo, and rsync against the mounted snapshot of one volume
	// can't reach the other regardless.
	for _, rp := range p.RestorePaths {
		if !literalContains(p.ManagedRoot, rp) {
			return fmt.Errorf("policy invalid: restore_path %q is not inside managed_root %q", rp, p.ManagedRoot)
		}
	}
	return nil
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
		// pathpkg.Join (POSIX) — these are macOS paths and must use
		// forward slashes regardless of where the binary is built.
		p.RestorePaths = []string{
			pathpkg.Join(p.ManagedRoot, "Users"),
			pathpkg.Join(p.ManagedRoot, "Applications"),
		}
	}
	if p.StateFile == "" {
		p.StateFile = "/var/lib/nivenia/state.json"
	}
	if p.LogFile == "" {
		p.LogFile = "/var/log/nivenia.log"
	}
	if err := p.validate(); err != nil {
		return p, err
	}
	return p, nil
}
