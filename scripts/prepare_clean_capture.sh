#!/usr/bin/env bash
set -euo pipefail

MODE="run"
if [[ "${1:-}" == "--preflight-only" ]]; then
  MODE="preflight"
fi

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

clear_dir_contents() {
  local path="$1"
  if [[ -d "$path" ]]; then
    find "$path" -mindepth 1 -maxdepth 1 -exec rm -rf {} + >/dev/null 2>&1 || true
  fi
}

ensure_owned_dir() {
  local user="$1"
  local path="$2"
  local group=""

  group="$(id -gn "$user" 2>/dev/null || echo staff)"
  mkdir -p "$path" >/dev/null 2>&1 || true
  chown "$user:$group" "$path" >/dev/null 2>&1 || true
  chmod 700 "$path" >/dev/null 2>&1 || true
}

list_real_users() {
  while read -r name uid; do
    if [[ "$uid" =~ ^[0-9]+$ ]] && (( uid >= 500 )) && [[ "$name" != "nobody" ]]; then
      echo "$name"
    fi
  done < <(dscl . -list /Users UniqueID 2>/dev/null || true)
}

user_home() {
  local user="$1"
  local home=""

  home="$(dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | awk '{print $2}' | head -n 1 || true)"
  if [[ -z "$home" ]]; then
    home="/Users/$user"
  fi

  if [[ -d "$home" ]]; then
    echo "$home"
  fi
}

verify_user_cache_ownership() {
  local user="$1"
  local home="$2"
  local cache_dir="$home/Library/Caches"
  local library_dir="$home/Library"
  local owner=""

  if ! id "$user" >/dev/null 2>&1; then
    log "preflight failed: user $user is not resolvable"
    return 1
  fi

  if [[ ! -d "$library_dir" ]]; then
    log "preflight failed: missing directory $library_dir"
    return 1
  fi

  owner="$(stat -f %Su "$library_dir" 2>/dev/null || true)"
  if [[ "$owner" != "$user" ]]; then
    log "preflight failed: $library_dir owner is '$owner', expected '$user'"
    return 1
  fi

  if [[ -e "$cache_dir" ]]; then
    owner="$(stat -f %Su "$cache_dir" 2>/dev/null || true)"
    if [[ "$owner" != "$user" ]]; then
      log "preflight failed: $cache_dir owner is '$owner', expected '$user'"
      return 1
    fi
  fi

  return 0
}

sanitize_user_home() {
  local user="$1"
  local home="$2"

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

  ensure_owned_dir "$user" "$home/Library/Caches"
  clear_dir_contents "$home/Library/Caches"
}

log "stopping browsers and user cache writers"
kill_if_running "Safari"
kill_if_running "Google Chrome"
kill_if_running "Microsoft Edge"
kill_if_running "firefox"
kill_if_running "cfprefsd"

preflight_ok=1
while IFS= read -r user; do
  home="$(user_home "$user")"
  if [[ -z "$home" || ! -d "$home" ]]; then
    continue
  fi

  if ! verify_user_cache_ownership "$user" "$home"; then
    preflight_ok=0
  fi

  if [[ "$MODE" == "preflight" ]]; then
    continue
  fi

  log "sanitizing user data for $user"
  sanitize_user_home "$user" "$home"
done < <(list_real_users)

if [[ "$preflight_ok" != "1" ]]; then
  log "preflight failed: refusing to run cleanup"
  exit 1
fi

if [[ "$MODE" == "preflight" ]]; then
  log "preflight checks passed"
  exit 0
fi

log "sanitizing system caches"
clear_path "/Library/Caches"
mkdir -p "/Library/Caches" >/dev/null 2>&1 || true
chown root:wheel "/Library/Caches" >/dev/null 2>&1 || true
chmod 755 "/Library/Caches" >/dev/null 2>&1 || true

log "pre-capture cleanup complete"
