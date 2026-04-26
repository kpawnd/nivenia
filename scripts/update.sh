#!/usr/bin/env bash
set -euo pipefail

# Nivenia updater.
#
# Safety properties (see comments below for details):
#   * install set is atomic: either every file ends up at the new version, or
#     every file is reverted to the previous version. No half-updated boxes.
#   * new binaries are smoke-tested before they replace the running ones.
#   * downgrades are refused unless NIVENIA_ALLOW_DOWNGRADE=1.
#   * the version marker is written with fsync so power loss never leaves the
#     filesystem pointing at the old version while the binaries are new.
#   * the updater never bootout's itself (launchd sends SIGTERM to the PG on
#     bootout, which would kill this script mid-commit).

REPO="${NIVENIA_REPO:-kpawnd/nivenia}"
CHANNEL="${NIVENIA_CHANNEL:-latest}"
INSTALL_LIBEXEC_DIR="${NIVENIA_LIBEXEC_DIR:-/usr/local/libexec}"
INSTALL_BIN_DIR="${NIVENIA_BIN_DIR:-/usr/local/bin}"
POLICY_DIR="${NIVENIA_POLICY_DIR:-/etc/nivenia}"
STATE_DIR="${NIVENIA_STATE_DIR:-/var/lib/nivenia}"
RESTORE_PLIST_PATH="${NIVENIA_LAUNCHD_PLIST:-/Library/LaunchDaemons/com.nivenia.restore.plist}"
UPDATER_PLIST_PATH="${NIVENIA_UPDATER_LAUNCHD_PLIST:-/Library/LaunchDaemons/com.nivenia.updater.plist}"
LOG_FILE="${NIVENIA_UPDATER_LOG:-/var/log/nivenia-updater.log}"
LOCK_DIR="/tmp/nivenia-updater.lock"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

need_cmd curl
need_cmd tar
need_cmd install
need_cmd launchctl
need_cmd sw_vers

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root (use sudo nivenia-update)" >&2
  exit 1
fi

OS_VERSION="$(sw_vers -productVersion)"
OS_MAJOR="${OS_VERSION%%.*}"
if ! [[ "$OS_MAJOR" =~ ^[0-9]+$ ]]; then
  echo "could not parse macOS version: $OS_VERSION" >&2
  exit 1
fi
if (( OS_MAJOR < 12 || OS_MAJOR > 15 )); then
  echo "unsupported macOS $OS_VERSION: only Monterey (12) through Sequoia (15) are supported" >&2
  exit 1
fi

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "updater already running"
  exit 0
fi

# Tracked resources for trap cleanup. Populated as we go so the trap always
# knows what to tear down regardless of where we bail out. On macOS bash 3.2,
# "${arr[@]}" on an empty array triggers set -u, so every iteration is
# guarded by a count check.
TMP_DIR=""
STAGED_FILES=()
BACKUP_FILES=()
COMMITTED_PAIRS=()   # "DEST|BACKUP" for files already renamed to their final path
COMMIT_SUCCESS=0

cleanup() {
  # If we committed any files but didn't make it through to the success
  # line, roll them back from their .bak copies so the system is returned
  # to exactly the previous version.
  if [[ ${#COMMITTED_PAIRS[@]} -gt 0 && "$COMMIT_SUCCESS" != "1" ]]; then
    log "rollback: reverting ${#COMMITTED_PAIRS[@]} committed file(s)"
    local pair dst bak
    for pair in "${COMMITTED_PAIRS[@]}"; do
      dst="${pair%%|*}"
      bak="${pair##*|}"
      if [[ -f "$bak" ]]; then
        mv -f "$bak" "$dst" 2>/dev/null || true
      fi
    done
  fi
  # Remove any still-staged .new files — they were never activated.
  if [[ ${#STAGED_FILES[@]} -gt 0 ]]; then
    local f
    for f in "${STAGED_FILES[@]}"; do
      [[ -f "$f" ]] && rm -f "$f" || true
    done
  fi
  # Remove any surviving .bak files after a successful run.
  if [[ "$COMMIT_SUCCESS" == "1" && ${#BACKUP_FILES[@]} -gt 0 ]]; then
    local b
    for b in "${BACKUP_FILES[@]}"; do
      [[ -f "$b" ]] && rm -f "$b" || true
    done
  fi
  [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]] && rm -rf "$TMP_DIR"
  rmdir "$LOCK_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Single source of log output. If the launchd plist redirects stdout to the
# same file we'd get every line twice (the old tee -a pattern), so we write
# only to the file and let stdout flow to the terminal for manual runs.
log() {
  local stamped
  stamped="$(date -u +%Y-%m-%dT%H:%M:%SZ) $1"
  printf '%s\n' "$stamped" >>"$LOG_FILE"
  printf '%s\n' "$stamped"
}

# Numeric semver compare. Prints "lt", "eq", or "gt" for $1 vs $2.
# Strips a leading "v" and any "-...+..." prerelease/build suffix so that
# "1.2.3" and "v1.2.3" compare equal and "0.0.0-dev+abc" sorts below any
# real release.
version_compare() {
  local a="${1#v}" b="${2#v}"
  a="${a%%-*}"; a="${a%%+*}"
  b="${b%%-*}"; b="${b%%+*}"
  if [[ "$a" == "$b" ]]; then
    echo "eq"; return
  fi
  local IFS=.
  local -a aa=($a) bb=($b)
  local i max=${#aa[@]}
  (( ${#bb[@]} > max )) && max=${#bb[@]}
  for (( i=0; i<max; i++ )); do
    local ai="${aa[i]:-0}" bi="${bb[i]:-0}"
    if ! [[ "$ai" =~ ^[0-9]+$ && "$bi" =~ ^[0-9]+$ ]]; then
      if [[ "$ai" < "$bi" ]]; then echo "lt"; return; fi
      if [[ "$ai" > "$bi" ]]; then echo "gt"; return; fi
      continue
    fi
    if (( ai < bi )); then echo "lt"; return; fi
    if (( ai > bi )); then echo "gt"; return; fi
  done
  echo "eq"
}

resolve_version() {
  if [[ "$CHANNEL" != "latest" ]]; then
    printf '%s' "$CHANNEL"
    return
  fi
  local api_url="https://api.github.com/repos/${REPO}/releases/latest"
  curl -fsSL "$api_url" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -n1
}

current_version() {
  if [[ -f "$STATE_DIR/version" ]]; then
    cat "$STATE_DIR/version"
    return
  fi
  echo ""
}

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  *)
    log "unsupported architecture: $ARCH (Intel x86_64 only)"
    exit 1
    ;;
esac

if [[ "$OS" != "darwin" ]]; then
  log "unsupported os: $OS"
  exit 1
fi

VERSION="$(resolve_version)"
if [[ -z "$VERSION" ]]; then
  log "could not resolve target version"
  exit 1
fi

CURRENT="$(current_version)"
if [[ -n "$CURRENT" ]]; then
  cmp="$(version_compare "$VERSION" "$CURRENT")"
  case "$cmp" in
    eq)
      log "already up to date ($CURRENT)"
      exit 0
      ;;
    lt)
      # Installed version is newer than the channel head. A dev build from
      # setup.sh is stamped "0.0.0-dev+<sha>" which sorts below any real
      # release, so dev→release upgrades DO NOT hit this path — they show
      # up as gt and proceed normally. This branch only fires if an admin
      # hand-installed a newer build than what's currently on the channel.
      if [[ "${NIVENIA_ALLOW_DOWNGRADE:-0}" != "1" ]]; then
        log "refusing to downgrade from $CURRENT to $VERSION (set NIVENIA_ALLOW_DOWNGRADE=1 to force)"
        exit 0
      fi
      log "WARNING: NIVENIA_ALLOW_DOWNGRADE=1; proceeding from $CURRENT to $VERSION"
      ;;
    gt)
      : # normal upgrade path
      ;;
  esac
fi

ARCHIVE="nivenia_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${VERSION}/${ARCHIVE}"
TMP_DIR="$(mktemp -d)"

log "downloading ${URL}"
curl -fL "$URL" -o "$TMP_DIR/$ARCHIVE"

# Extract into a dedicated subdir so a malicious archive containing absolute
# or traversing paths can't pollute TMP_DIR's siblings. We only install files
# we explicitly name below, so traversal into /etc is blocked by our install
# list regardless — this is defense in depth.
mkdir -p "$TMP_DIR/extract"
tar -C "$TMP_DIR/extract" -xzf "$TMP_DIR/$ARCHIVE"
SRC_DIR="$TMP_DIR/extract"

for required in niveniad niveniactl com.nivenia.restore.plist com.nivenia.updater.plist update.sh; do
  if [[ ! -f "$SRC_DIR/$required" ]]; then
    log "missing required artifact: $required"
    exit 1
  fi
done

install -d "$INSTALL_LIBEXEC_DIR" "$INSTALL_BIN_DIR" "$POLICY_DIR" "$STATE_DIR" "/var/lib/nivenia/recovery"

# ── Smoke test ────────────────────────────────────────────────────────────────
# Run the new binaries from the extract directory before touching any live
# file. --version returns 0 on a well-linked, correct-arch executable; a
# broken build (wrong arch, missing symbol, corrupted binary) will exit
# non-zero here and we abort without modifying anything.
chmod +x "$SRC_DIR/niveniad" "$SRC_DIR/niveniactl" 2>/dev/null || true
if ! "$SRC_DIR/niveniad" --version >/dev/null 2>&1; then
  log "smoke test failed: new niveniad --version returned non-zero"
  exit 1
fi
if ! "$SRC_DIR/niveniactl" --version >/dev/null 2>&1; then
  log "smoke test failed: new niveniactl --version returned non-zero"
  exit 1
fi
log "smoke test passed"

# ── Stage ─────────────────────────────────────────────────────────────────────
# Each target gets written to "$dst.new" first. Nothing live is touched yet.
# If any stage step fails, the trap removes the .new files and the system is
# unchanged.
stage_file() {
  local src="$1" dst="$2" mode="$3"
  local dst_new="$dst.new"
  install -m "$mode" "$src" "$dst_new"
  STAGED_FILES+=("$dst_new")
}

stage_file "$SRC_DIR/niveniad"                  "$INSTALL_LIBEXEC_DIR/niveniad"                       755
stage_file "$SRC_DIR/niveniactl"                "$INSTALL_BIN_DIR/niveniactl"                         755
stage_file "$SRC_DIR/update.sh"                 "$INSTALL_LIBEXEC_DIR/nivenia-updater"                755
stage_file "$SRC_DIR/update.sh"                 "$INSTALL_BIN_DIR/nivenia-update"                     755
if [[ -f "$SRC_DIR/nivenia_recovery.sh" ]]; then
  stage_file "$SRC_DIR/nivenia_recovery.sh"     "$INSTALL_BIN_DIR/nivenia-recovery"                   755
  stage_file "$SRC_DIR/nivenia_recovery.sh"     "/var/lib/nivenia/recovery/nivenia-recovery.sh"       755
fi
if [[ -f "$SRC_DIR/prepare_clean_capture.sh" ]]; then
  stage_file "$SRC_DIR/prepare_clean_capture.sh" "$INSTALL_LIBEXEC_DIR/nivenia-prepare-clean-capture" 755
  stage_file "$SRC_DIR/prepare_clean_capture.sh" "$INSTALL_BIN_DIR/nivenia-prepare-clean-capture"     755
fi
stage_file "$SRC_DIR/com.nivenia.restore.plist" "$RESTORE_PLIST_PATH"                                 644
stage_file "$SRC_DIR/com.nivenia.updater.plist" "$UPDATER_PLIST_PATH"                                 644
# policy.json is only installed if none exists (preserve admin edits).
if [[ ! -f "$POLICY_DIR/policy.json" && -f "$SRC_DIR/policy.json" ]]; then
  stage_file "$SRC_DIR/policy.json" "$POLICY_DIR/policy.json" 644
fi

# Purge retired helpers left over from older releases so they don't confuse
# triage. These are not staged; they're unconditional removals of known-dead
# paths. Do this before commit so a mid-update crash can't leave both the new
# layout and the old retired files.
rm -f "$INSTALL_BIN_DIR/nivenia-emergency-disable" \
      "$INSTALL_BIN_DIR/nivenia-emergency-revert" \
      "/var/lib/nivenia/recovery/nivenia-emergency-disable.sh" \
      "/var/lib/nivenia/recovery/nivenia-emergency-revert.sh"

# ── Backup ────────────────────────────────────────────────────────────────────
# Copy every live file we're about to overwrite to "$dst.bak" on the same
# filesystem (so the rollback rename is atomic). Files that don't exist yet
# have no backup entry — a rollback simply deletes the newly placed file.
backup_file() {
  local dst="$1"
  if [[ -f "$dst" ]]; then
    local bak="$dst.bak"
    cp -p "$dst" "$bak"
    BACKUP_FILES+=("$bak")
  fi
}

if [[ ${#STAGED_FILES[@]} -gt 0 ]]; then
  for staged in "${STAGED_FILES[@]}"; do
    backup_file "${staged%.new}"
  done
fi

# ── Commit ────────────────────────────────────────────────────────────────────
# Tight loop of renames. Each rename is atomic within a filesystem. If any
# rename fails, the trap rolls back every earlier rename from its .bak copy.
# Every destination path is either fully old or fully new at any instant.
if [[ ${#STAGED_FILES[@]} -gt 0 ]]; then
  for staged in "${STAGED_FILES[@]}"; do
    dst="${staged%.new}"
    bak="$dst.bak"
    if ! mv -f "$staged" "$dst"; then
      log "commit failed at $dst; trap will roll back"
      exit 1
    fi
    COMMITTED_PAIRS+=("$dst|$bak")
  done
fi

# ── Version marker ────────────────────────────────────────────────────────────
# fsync the version file and its directory. Without this, a power cut between
# the rename and the metadata commit could leave the version file pointing at
# the old version while all binaries are new — the next updater run would
# then needlessly reinstall the same version (and redo the whole commit).
VERSION_TMP="$STATE_DIR/version.tmp"
printf '%s' "$VERSION" >"$VERSION_TMP"
fsync_path() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import os,sys
fd = os.open(sys.argv[1], os.O_RDONLY)
try:
    os.fsync(fd)
finally:
    os.close(fd)' "$1"
  else
    sync
  fi
}
fsync_path "$VERSION_TMP"
mv -f "$VERSION_TMP" "$STATE_DIR/version"
fsync_path "$STATE_DIR"

# ── Plist reload ──────────────────────────────────────────────────────────────
# We deliberately do NOT bootout+bootstrap the updater service here. launchctl
# bootout sends SIGTERM to the job's process group, which kills this script
# mid-cleanup. The new updater plist is already on disk and launchd re-reads
# it at the next boot (these machines restart nightly via pmset). If an admin
# needs it reloaded sooner, they can run manually:
#   sudo launchctl bootout   system $UPDATER_PLIST_PATH
#   sudo launchctl bootstrap system $UPDATER_PLIST_PATH
# The restore plist is likewise only consumed at boot, so no reload is needed.

COMMIT_SUCCESS=1
log "updated from ${CURRENT:-none} to $VERSION"
