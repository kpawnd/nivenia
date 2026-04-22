#!/usr/bin/env bash
# Triggered nightly at 03:00 by com.nivenia.scheduled-restart.
# Restarts only when no interactive user session is active so we never
# forcibly kill a student mid-session; the restart happens on the next
# 03:00 window after the machine is idle.
set -euo pipefail

LOGFILE="/var/log/nivenia.log"

log() {
  printf '%s scheduled-restart: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "$LOGFILE"
}

# stat -f '%Su' /dev/console returns the current console user on macOS.
# It is "_windowserver" or empty when no user is logged in.
CONSOLE_USER="$(stat -f '%Su' /dev/console 2>/dev/null || true)"

if [[ -n "$CONSOLE_USER" && "$CONSOLE_USER" != "root" && "$CONSOLE_USER" != "_windowserver" ]]; then
  log "deferred: $CONSOLE_USER is active on console"
  exit 0
fi

log "no active console session — restarting now"
shutdown -r now
