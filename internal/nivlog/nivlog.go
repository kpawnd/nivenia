// Package nivlog is the single structured-log writer used by niveniad,
// niveniactl, and the engine. Every log line is one event, written as
// key=value pairs prefixed with a UTC timestamp and severity.
//
// Format:
//
//	t=2026-04-26T12:56:53Z level=INFO event=restore.start session=abc123 ...
//
// Goals (driven by the boot-restore regression debugging this replaces):
//
//   - Every line is parseable (one event per line, no embedded newlines).
//   - Every line includes the session ID so admin can grep for one boot's
//     full timeline. The session ID is stable across niveniad's run.
//   - Errors include enough context to diagnose without re-running. When
//     a subprocess fails (rsync, diskutil, mount_apfs), we record the
//     full command, exit code, and stderr file path — not a one-liner
//     stripped of context.
//   - For long sub-process output (rsync stderr can be hundreds of lines
//     on partial failure), we write the full output to a per-run file
//     under /var/log/nivenia/ and reference it from the log line via
//     a `detail=...` field. The main log stays scannable; the detail
//     file has the full transcript for triage.
//
// This package intentionally has no dependencies beyond the standard
// library so it can be used from every other package without import
// cycles.
package nivlog

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level enumerates the severities in use. We deliberately keep this
// short — every log line is consumed by humans first, automation
// second, and adding more levels makes greps less reliable.
type Level string

const (
	LevelInfo  Level = "INFO"
	LevelWarn  Level = "WARN"
	LevelError Level = "ERROR"
)

// Logger writes structured log events to one or more sinks. The zero
// value is unusable — call New() to construct.
type Logger struct {
	mu        sync.Mutex
	primary   string // primary log file (typically /var/log/nivenia.log)
	detailDir string // directory for per-run detail files
	session   string // 8-hex-char ID stable for the lifetime of this Logger
	component string // "niveniad" / "niveniactl" / "test"
	version   string // build version (passed in by main)
	osVersion string // sw_vers -productVersion at startup, for boot records
}

// New returns a Logger ready to write to primary. detailDir, if
// non-empty, will be created on first detail write so subprocess
// transcripts can be persisted alongside the main log.
//
// session is generated automatically; pass an empty component name if
// the caller doesn't have one yet (it can be set later via WithFields).
func New(primary, detailDir, component, version, osVersion string) *Logger {
	return &Logger{
		primary:   primary,
		detailDir: detailDir,
		session:   newSessionID(),
		component: component,
		version:   version,
		osVersion: osVersion,
	}
}

func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Time-based fallback: low-resolution but never empty, so
		// a logger that fails to seed still distinguishes runs.
		return fmt.Sprintf("t%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// Session returns this logger's session ID so callers (e.g. the engine)
// can surface it in user-facing messages.
func (l *Logger) Session() string {
	return l.session
}

// Event writes one structured log line. Pass key/value pairs for the
// event-specific data; system fields (t, level, event, session,
// component, version, os) are added automatically.
//
// Values are escaped: any value containing whitespace, quotes, or
// equals-signs is wrapped in double-quotes with internal quotes
// backslash-escaped. This keeps every log line as one record.
func (l *Logger) Event(level Level, event string, kv ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	var b strings.Builder
	fmt.Fprintf(&b, "t=%s level=%s event=%s", now, level, event)
	if l.session != "" {
		fmt.Fprintf(&b, " session=%s", l.session)
	}
	if l.component != "" {
		fmt.Fprintf(&b, " component=%s", l.component)
	}
	if l.version != "" {
		fmt.Fprintf(&b, " version=%s", l.version)
	}

	for i := 0; i+1 < len(kv); i += 2 {
		key := fmt.Sprint(kv[i])
		val := fmt.Sprint(kv[i+1])
		fmt.Fprintf(&b, " %s=%s", key, escapeValue(val))
	}
	b.WriteByte('\n')

	line := b.String()

	// Always echo to stderr so launchd captures the line in
	// niveniad.err.log even if the primary file isn't writable yet.
	fmt.Fprint(os.Stderr, line)

	if l.primary != "" {
		_ = os.MkdirAll(filepath.Dir(l.primary), 0o755)
		if f, err := os.OpenFile(l.primary, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
	}
}

// Info / Warn / Error are convenience wrappers.
func (l *Logger) Info(event string, kv ...any)  { l.Event(LevelInfo, event, kv...) }
func (l *Logger) Warn(event string, kv ...any)  { l.Event(LevelWarn, event, kv...) }
func (l *Logger) Error(event string, kv ...any) { l.Event(LevelError, event, kv...) }

// WriteDetail saves a transcript to a per-run file under detailDir
// and returns the file's absolute path. Callers reference the path
// from a log event via `detail=...` so the main log line stays a
// single record while the full output is available for triage.
//
// The filename includes timestamp, event tag, and session ID so it
// sorts naturally and is easy to correlate.
func (l *Logger) WriteDetail(tag, content string) string {
	if l == nil || l.detailDir == "" {
		return ""
	}
	if err := os.MkdirAll(l.detailDir, 0o755); err != nil {
		return ""
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	safeTag := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, tag)
	if safeTag == "" {
		safeTag = "detail"
	}
	name := fmt.Sprintf("%s-%s-%s.log", stamp, safeTag, l.session)
	path := filepath.Join(l.detailDir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return ""
	}
	return path
}

// escapeValue returns v as-is when it's a "simple" token, or wrapped
// in double quotes (with internal quotes backslash-escaped) when it
// contains anything that would break key=value parsing.
func escapeValue(v string) string {
	if v == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range v {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '=' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
