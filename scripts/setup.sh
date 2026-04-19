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
  echo "clearing user session/cache data before capture..."
  sudo /usr/local/bin/nivenia-prepare-clean-capture
else
  echo "skipping pre-capture cleanup (NIVENIA_SKIP_PRECAPTURE_CLEAN=1)"
fi

echo "capturing baseline and enabling frozen mode..."
sudo /usr/local/bin/niveniactl freeze --policy "$POLICY_PATH" --state "$STATE_PATH"

echo "starting launch daemon..."
sudo launchctl bootout system "$DAEMON_PATH" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$DAEMON_PATH"
sudo launchctl bootout system "$UPDATER_DAEMON_PATH" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$UPDATER_DAEMON_PATH"

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
