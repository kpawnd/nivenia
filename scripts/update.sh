#!/usr/bin/env bash
set -euo pipefail

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
trap 'rmdir "$LOCK_DIR" >/dev/null 2>&1 || true' EXIT

log() {
  local msg="$1"
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $msg" | tee -a "$LOG_FILE"
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
if [[ "$CURRENT" == "$VERSION" ]]; then
  log "already up to date ($CURRENT)"
  exit 0
fi

ARCHIVE="nivenia_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${VERSION}/${ARCHIVE}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"; rmdir "$LOCK_DIR" >/dev/null 2>&1 || true' EXIT

log "downloading ${URL}"
curl -fL "$URL" -o "$TMP_DIR/$ARCHIVE"

tar -C "$TMP_DIR" -xzf "$TMP_DIR/$ARCHIVE"

for required in niveniad niveniactl com.nivenia.restore.plist com.nivenia.updater.plist update.sh; do
  if [[ ! -f "$TMP_DIR/$required" ]]; then
    log "missing required artifact: $required"
    exit 1
  fi
done

install -d "$INSTALL_LIBEXEC_DIR" "$INSTALL_BIN_DIR" "$POLICY_DIR" "$STATE_DIR" "/var/lib/nivenia/recovery"
install -m 755 "$TMP_DIR/niveniad" "$INSTALL_LIBEXEC_DIR/niveniad"
install -m 755 "$TMP_DIR/niveniactl" "$INSTALL_BIN_DIR/niveniactl"
install -m 755 "$TMP_DIR/update.sh" "$INSTALL_LIBEXEC_DIR/nivenia-updater"
install -m 755 "$TMP_DIR/update.sh" "$INSTALL_BIN_DIR/nivenia-update"

if [[ -f "$TMP_DIR/nivenia_recovery.sh" ]]; then
  install -m 755 "$TMP_DIR/nivenia_recovery.sh" "$INSTALL_BIN_DIR/nivenia-recovery"
  install -m 755 "$TMP_DIR/nivenia_recovery.sh" "/var/lib/nivenia/recovery/nivenia-recovery.sh"
fi

rm -f "$INSTALL_BIN_DIR/nivenia-emergency-disable" "$INSTALL_BIN_DIR/nivenia-emergency-revert"
rm -f "/var/lib/nivenia/recovery/nivenia-emergency-disable.sh" "/var/lib/nivenia/recovery/nivenia-emergency-revert.sh"

if [[ ! -f "$POLICY_DIR/policy.json" && -f "$TMP_DIR/policy.json" ]]; then
  install -m 644 "$TMP_DIR/policy.json" "$POLICY_DIR/policy.json"
fi

install -m 644 "$TMP_DIR/com.nivenia.restore.plist" "$RESTORE_PLIST_PATH"
install -m 644 "$TMP_DIR/com.nivenia.updater.plist" "$UPDATER_PLIST_PATH"

echo "$VERSION" > "$STATE_DIR/version"

launchctl bootout system "$UPDATER_PLIST_PATH" >/dev/null 2>&1 || true
launchctl bootstrap system "$UPDATER_PLIST_PATH"

log "updated from ${CURRENT:-none} to $VERSION"
