#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY_PATH="${NIVENIA_POLICY_PATH:-/etc/nivenia/policy.json}"
STATE_PATH="${NIVENIA_STATE_PATH:-/var/lib/nivenia/state.json}"
DAEMON_PATH="/Library/LaunchDaemons/com.nivenia.restore.plist"
UPDATER_DAEMON_PATH="/Library/LaunchDaemons/com.nivenia.updater.plist"

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

need_cmd go
need_cmd sudo
need_cmd launchctl
need_cmd rsync
need_cmd sw_vers

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

cd "$REPO_ROOT"

GO_CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/nivenia-go-build"
mkdir -p "$GO_CACHE_DIR"
export GOCACHE="$GO_CACHE_DIR"

UPDATE_SCRIPT_SOURCE="scripts/update.sh"
if [[ ! -f "$UPDATE_SCRIPT_SOURCE" ]]; then
  UPDATE_SCRIPT_SOURCE="update.sh"
fi

EMERGENCY_SCRIPT_SOURCE="scripts/emergency_recovery_disable.sh"
if [[ ! -f "$EMERGENCY_SCRIPT_SOURCE" ]]; then
  EMERGENCY_SCRIPT_SOURCE="emergency_recovery_disable.sh"
fi

PREPARE_CLEAN_CAPTURE_SOURCE="scripts/prepare_clean_capture.sh"
if [[ ! -f "$PREPARE_CLEAN_CAPTURE_SOURCE" ]]; then
  PREPARE_CLEAN_CAPTURE_SOURCE="prepare_clean_capture.sh"
fi

echo "building niveniad and niveniactl..."
go build -o niveniad ./cmd/niveniad
go build -o niveniactl ./cmd/niveniactl

echo "installing binaries, updater, and policy..."
sudo install -d /usr/local/libexec /usr/local/bin /etc/nivenia /var/lib/nivenia /var/lib/nivenia/recovery
sudo install -m 755 niveniad /usr/local/libexec/niveniad
sudo install -m 755 niveniactl /usr/local/bin/niveniactl
sudo install -m 755 "$UPDATE_SCRIPT_SOURCE" /usr/local/libexec/nivenia-updater
sudo install -m 755 "$UPDATE_SCRIPT_SOURCE" /usr/local/bin/nivenia-update
sudo install -m 755 "$EMERGENCY_SCRIPT_SOURCE" /usr/local/bin/nivenia-emergency-disable
sudo install -m 755 "$EMERGENCY_SCRIPT_SOURCE" /var/lib/nivenia/recovery/nivenia-emergency-disable.sh
sudo install -m 755 "$PREPARE_CLEAN_CAPTURE_SOURCE" /usr/local/libexec/nivenia-prepare-clean-capture
sudo install -m 755 "$PREPARE_CLEAN_CAPTURE_SOURCE" /usr/local/bin/nivenia-prepare-clean-capture
sudo install -m 644 configs/policy.json "$POLICY_PATH"
sudo install -m 644 launchd/com.nivenia.restore.plist "$DAEMON_PATH"
sudo install -m 644 launchd/com.nivenia.updater.plist "$UPDATER_DAEMON_PATH"

if [[ "${NIVENIA_SKIP_PRECAPTURE_CLEAN:-0}" != "1" ]]; then
  echo "checking pre-capture cleanup safety..."
  if sudo /usr/local/bin/nivenia-prepare-clean-capture --preflight-only; then
    echo "clearing user session/cache data before capture..."
    if ! sudo /usr/local/bin/nivenia-prepare-clean-capture; then
      echo "warning: pre-capture cleanup failed; continuing with capture" >&2
    fi
  else
    echo "warning: pre-capture cleanup skipped (ownership preflight failed)" >&2
    echo "warning: fix user cache ownership or set NIVENIA_SKIP_PRECAPTURE_CLEAN=1" >&2
  fi
else
  echo "skipping pre-capture cleanup (NIVENIA_SKIP_PRECAPTURE_CLEAN=1)"
fi

echo "capturing baseline and enabling frozen mode..."
sudo /usr/local/bin/niveniactl freeze --policy "$POLICY_PATH" --state "$STATE_PATH"

echo "starting launch daemon..."
sudo rm -f /var/lib/nivenia/restore.lock
sudo launchctl bootout system "$DAEMON_PATH" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$DAEMON_PATH"
sudo launchctl bootout system "$UPDATER_DAEMON_PATH" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$UPDATER_DAEMON_PATH"

echo "verifying restore daemon..."
sudo launchctl kickstart -k system/com.nivenia.restore >/dev/null 2>&1 || {
  echo "failed to kickstart com.nivenia.restore" >&2
  exit 1
}

verify_ok=0
for _ in $(seq 1 30); do
  status_line="$(sudo /usr/local/bin/niveniactl status --state "$STATE_PATH" 2>/dev/null || true)"
  if [[ "$status_line" == *'mode=frozen'* && "$status_line" == *'last_restore_ok=true'* && "$status_line" == *'message="restore completed"'* ]]; then
    verify_ok=1
    break
  fi
  sleep 1
done

if [[ "$verify_ok" != "1" ]]; then
  echo "restore verification failed; check logs:" >&2
  echo "  sudo tail -n 120 /var/log/nivenia.log" >&2
  echo "  sudo tail -n 120 /var/log/niveniad.err.log" >&2
  exit 1
fi

echo "done"
echo "status:"
sudo /usr/local/bin/niveniactl status --state "$STATE_PATH"
echo "thaw temporarily: sudo niveniactl thaw-once"
echo "thaw until refreeze: sudo niveniactl thaw"
echo "refreeze now: sudo niveniactl freeze --policy $POLICY_PATH --state $STATE_PATH"
echo "manual update: sudo nivenia-update"
echo "emergency disable: sudo nivenia-emergency-disable"
echo "manual pre-capture cleanup: sudo nivenia-prepare-clean-capture"
echo "recovery script: /var/lib/nivenia/recovery/nivenia-emergency-disable.sh"
