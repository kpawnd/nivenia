//go:build windows

package engine

// pidIsAlive is not meaningfully implementable on Windows with POSIX semantics.
// Return true (conservative) so the mtime-based window in lockLooksActive still
// catches stale locks. This is a non-issue in production (darwin only).
func pidIsAlive(_ int) bool {
	return true
}
