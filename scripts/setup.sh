#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY_PATH="${NIVENIA_POLICY_PATH:-/etc/nivenia/policy.json}"
STATE_PATH="${NIVENIA_STATE_PATH:-/var/lib/nivenia/state.json}"
DAEMON_PATH="/Library/LaunchDaemons/com.nivenia.restore.plist"
UPDATER_DAEMON_PATH="/Library/LaunchDaemons/com.nivenia.updater.plist"

# ── colors ───────────────────────────────────────────────────────────────────
if [ -t 1 ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RESET=$'\033[0m'
  RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'
  CYAN=$'\033[36m'; BLUE=$'\033[34m'; WHITE=$'\033[97m'
else
  BOLD=''; DIM=''; RESET=''; RED=''; GREEN=''; YELLOW=''
  CYAN=''; BLUE=''; WHITE=''
fi

step()  { printf '\n%s▶  %s%s\n' "${BOLD}${CYAN}" "$*" "${RESET}"; }
ok()    { printf '%s✓  %s%s\n'   "${GREEN}"        "$*" "${RESET}"; }
warn()  { printf '%s⚠  %s%s\n'   "${YELLOW}"       "$*" "${RESET}" >&2; }
fail()  { printf '%s✗  %s%s\n'   "${RED}"           "$*" "${RESET}" >&2; }
info()  { printf '%s   %s%s\n'   "${DIM}"           "$*" "${RESET}"; }
# ─────────────────────────────────────────────────────────────────────────────

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    fail "missing required command: $1"
    exit 1
  fi
}

need_cmd go
need_cmd sudo
need_cmd launchctl
need_cmd diskutil
need_cmd sw_vers

OS_VERSION="$(sw_vers -productVersion)"
OS_MAJOR="${OS_VERSION%%.*}"
if ! [[ "$OS_MAJOR" =~ ^[0-9]+$ ]]; then
  fail "could not parse macOS version: $OS_VERSION"
  exit 1
fi
if (( OS_MAJOR < 12 || OS_MAJOR > 15 )); then
  fail "unsupported macOS $OS_VERSION: only Monterey (12) through Sequoia (15) are supported"
  exit 1
fi

cd "$REPO_ROOT"

GO_CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/nivenia-go-build"
mkdir -p "$GO_CACHE_DIR"
export GOCACHE="$GO_CACHE_DIR"

UPDATE_SCRIPT_SOURCE="scripts/update.sh"
[[ -f "$UPDATE_SCRIPT_SOURCE" ]] || UPDATE_SCRIPT_SOURCE="update.sh"

RECOVERY_SCRIPT_SOURCE="scripts/nivenia_recovery.sh"
[[ -f "$RECOVERY_SCRIPT_SOURCE" ]] || RECOVERY_SCRIPT_SOURCE="nivenia_recovery.sh"

PREPARE_CLEAN_CAPTURE_SOURCE="scripts/prepare_clean_capture.sh"
[[ -f "$PREPARE_CLEAN_CAPTURE_SOURCE" ]] || PREPARE_CLEAN_CAPTURE_SOURCE="prepare_clean_capture.sh"

step "Building niveniad and niveniactl"
go build -o niveniad ./cmd/niveniad
go build -o niveniactl ./cmd/niveniactl
ok "Build complete"

step "Installing binaries, updater, and policy"
sudo install -d /usr/local/libexec /usr/local/bin /etc/nivenia /var/lib/nivenia /var/lib/nivenia/recovery
sudo install -m 755 niveniad  /usr/local/libexec/niveniad
sudo install -m 755 niveniactl /usr/local/bin/niveniactl
sudo install -m 755 "$UPDATE_SCRIPT_SOURCE"            /usr/local/libexec/nivenia-updater
sudo install -m 755 "$UPDATE_SCRIPT_SOURCE"            /usr/local/bin/nivenia-update
if [[ -f "$RECOVERY_SCRIPT_SOURCE" ]]; then
  sudo install -m 755 "$RECOVERY_SCRIPT_SOURCE" /usr/local/bin/nivenia-recovery
  sudo install -m 755 "$RECOVERY_SCRIPT_SOURCE" /var/lib/nivenia/recovery/nivenia-recovery.sh
fi
sudo rm -f /usr/local/bin/nivenia-emergency-disable /usr/local/bin/nivenia-emergency-revert
sudo rm -f /var/lib/nivenia/recovery/nivenia-emergency-disable.sh /var/lib/nivenia/recovery/nivenia-emergency-revert.sh
sudo install -m 755 "$PREPARE_CLEAN_CAPTURE_SOURCE" /usr/local/libexec/nivenia-prepare-clean-capture
sudo install -m 755 "$PREPARE_CLEAN_CAPTURE_SOURCE" /usr/local/bin/nivenia-prepare-clean-capture
sudo install -m 644 configs/policy.json "$POLICY_PATH"
sudo install -m 644 launchd/com.nivenia.restore.plist          "$DAEMON_PATH"
sudo install -m 644 launchd/com.nivenia.updater.plist          "$UPDATER_DAEMON_PATH"
# Remove scheduled-restart daemon if upgrading from a version that installed it;
# scheduled power-on/off is handled by pmset instead.
sudo launchctl bootout system /Library/LaunchDaemons/com.nivenia.scheduled-restart.plist >/dev/null 2>&1 || true
sudo rm -f /Library/LaunchDaemons/com.nivenia.scheduled-restart.plist
sudo rm -f /usr/local/libexec/nivenia-scheduled-restart
ok "Installation complete"

step "Configuring log rotation"
sudo tee /etc/newsyslog.d/nivenia.conf >/dev/null <<'NEWSYSLOG'
# Nivenia log rotation — 7 files, rotate at 5 MB each
/var/log/nivenia.log      root:admin  644  7  5120  *  J
/var/log/niveniad.out.log root:admin  644  7  5120  *  J
/var/log/niveniad.err.log root:admin  644  7  5120  *  J
NEWSYSLOG
ok "Log rotation configured"

step "Pre-capture cleanup preflight"
if ! sudo /usr/local/bin/nivenia-prepare-clean-capture --preflight-only; then
  fail "preflight failed — fix ownership before continuing"
  exit 1
fi

step "Clearing user session and cache data"
if ! sudo /usr/local/bin/nivenia-prepare-clean-capture; then
  fail "cleanup failed — refusing to capture baseline"
  exit 1
fi

step "Capturing baseline and enabling frozen mode"
sudo /usr/local/bin/niveniactl --policy "$POLICY_PATH" --state "$STATE_PATH" freeze
ok "Baseline captured"

step "Verifying restore"
info "Running a restore from the frozen snapshot to confirm everything works..."
sudo rm -f /var/lib/nivenia/restore.lock
if ! sudo /usr/local/libexec/niveniad --policy "$POLICY_PATH"; then
  fail "Restore verification failed — logs:"
  sudo tail -n 60 /var/log/niveniad.err.log 2>/dev/null >&2 || true
  sudo tail -n 20 /var/log/nivenia.log       2>/dev/null >&2 || true
  exit 1
fi
ok "Restore verified"

step "Registering launch daemons"
sudo launchctl bootout system "$DAEMON_PATH"         >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$DAEMON_PATH"
sudo launchctl bootout system "$UPDATER_DAEMON_PATH" >/dev/null 2>&1 || true
sudo launchctl bootstrap system "$UPDATER_DAEMON_PATH"
ok "Launch daemons registered"

sudo /usr/local/bin/niveniactl --state "$STATE_PATH" status

printf '%s  Quick reference%s\n' "${BOLD}" "${RESET}"
printf '  %s%-36s%s %s\n' "${DIM}" "thaw temporarily"         "${RESET}" "sudo niveniactl thaw-once"
printf '  %s%-36s%s %s\n' "${DIM}" "thaw until refreeze"      "${RESET}" "sudo niveniactl thaw"
printf '  %s%-36s%s %s\n' "${DIM}" "refreeze"                 "${RESET}" "sudo niveniactl --policy $POLICY_PATH --state $STATE_PATH freeze"
printf '  %s%-36s%s %s\n' "${DIM}" "manual update"            "${RESET}" "sudo nivenia-update"
printf '  %s%-36s%s %s\n' "${DIM}" "emergency disable"        "${RESET}" "sudo nivenia-recovery disable"
printf '  %s%-36s%s %s\n' "${DIM}" "emergency revert"         "${RESET}" "sudo nivenia-recovery revert"
printf '\n'
