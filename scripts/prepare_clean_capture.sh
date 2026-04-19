#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root (use sudo)" >&2
  exit 1
fi

log() {
  echo "[prep] $1"
}

kill_if_running() {
  local proc="$1"
  pkill -x "$proc" >/dev/null 2>&1 || true
}

clear_path() {
  local path="$1"
  if [[ -e "$path" ]]; then
    rm -rf "$path" >/dev/null 2>&1 || true
  fi
}

list_real_users() {
  dscl . -list /Users UniqueID | while read -r name uid; do
    if [[ "$uid" =~ ^[0-9]+$ ]] && (( uid >= 500 )) && [[ "$name" != "nobody" ]]; then
      echo "$name"
    fi
  done
}

sanitize_user_home() {
  local home="$1"

  clear_path "$home/Library/Safari"
  clear_path "$home/Library/WebKit/com.apple.Safari"
  clear_path "$home/Library/Cookies"
  clear_path "$home/Library/HTTPStorages"
  clear_path "$home/Library/Containers/com.apple.Safari"
  clear_path "$home/Library/Containers/com.apple.Safari.SafeBrowsing.Service"
  clear_path "$home/Library/Preferences/com.apple.Safari.plist"

  clear_path "$home/Library/Application Support/Google/Chrome/Default/Cookies"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Login Data"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Web Data"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/History"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Current Session"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Current Tabs"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Last Session"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Last Tabs"
  clear_path "$home/Library/Application Support/Google/Chrome/Default/Sessions"

  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Cookies"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Login Data"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Web Data"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/History"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Current Session"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Current Tabs"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Last Session"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Last Tabs"
  clear_path "$home/Library/Application Support/Microsoft Edge/Default/Sessions"

  clear_path "$home/Library/Application Support/Firefox/Profiles"

  clear_path "$home/Library/Caches"
  mkdir -p "$home/Library/Caches" >/dev/null 2>&1 || true
}

log "stopping browsers and user cache writers"
kill_if_running "Safari"
kill_if_running "Google Chrome"
kill_if_running "Microsoft Edge"
kill_if_running "firefox"
kill_if_running "cfprefsd"

for user in $(list_real_users); do
  home="$(dscl . -read /Users/"$user" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  if [[ -z "$home" || ! -d "$home" ]]; then
    continue
  fi
  log "sanitizing user data for $user"
  sanitize_user_home "$home"
done

log "sanitizing system caches"
clear_path "/Library/Caches"
mkdir -p "/Library/Caches" >/dev/null 2>&1 || true

log "pre-capture cleanup complete"
