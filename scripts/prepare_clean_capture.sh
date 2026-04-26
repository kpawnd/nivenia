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

if [ -t 1 ]; then
  _PREP_DIM=$'\033[2m'; _PREP_RESET=$'\033[0m'
else
  _PREP_DIM=''; _PREP_RESET=''
fi
log() {
  printf '%s[prep] %s%s\n' "${_PREP_DIM}" "$1" "${_PREP_RESET}"
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

  clear_dir_contents "$home/Downloads"
  clear_dir_contents "$home/Documents"

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

  # Collaboration caches
  clear_path "$home/Library/Application Support/Microsoft/Teams"
  clear_path "$home/Library/Application Support/Microsoft/Teams/Cache"
  clear_path "$home/Library/Group Containers/UBF8T346G9.com.microsoft.teams"
  clear_path "$home/Library/Group Containers/UBF8T346G9.com.microsoft.teams.shared"
  clear_path "$home/Library/Containers/com.microsoft.teams2"

  # Microsoft 365
  clear_path "$home/Library/Group Containers/UBF8T346G9.Office"
  clear_path "$home/Library/Group Containers/UBF8T346G9.OfficeOsfWebHost"
  clear_path "$home/Library/Group Containers/UBF8T346G9.OneDriveStandaloneSuite"
  clear_path "$home/Library/Preferences/com.microsoft.office.plist"

  # Adobe Creative Cloud
  clear_path "$home/Library/Application Support/Adobe"
  clear_path "$home/Library/Caches/Adobe"
  clear_path "$home/Library/Preferences/Adobe"

  # Blender
  clear_path "$home/Library/Application Support/Blender"
  clear_path "$home/Library/Preferences/org.blenderfoundation.blender.plist"

  # Azure Data Studio
  clear_path "$home/Library/Application Support/azuredatastudio"
  clear_path "$home/Library/Application Support/Azure Data Studio"
  clear_path "$home/Library/Caches/com.microsoft.azuredatastudio"
  clear_path "$home/Library/Preferences/com.microsoft.azuredatastudio.plist"

  # Android Studio
  for path in "$home/Library/Application Support/Google/AndroidStudio"*; do
    clear_path "$path"
  done
  for path in "$home/Library/Preferences/AndroidStudio"*; do
    clear_path "$path"
  done
  for path in "$home/Library/Caches/AndroidStudio"*; do
    clear_path "$path"
  done

  # Cisco Packet Tracer
  clear_path "$home/Library/Application Support/Cisco Packet Tracer"
  clear_path "$home/Library/Preferences/com.cisco.packettracer.plist"

  # ── Code editors and IDEs ──────────────────────────────────────────────────
  # VS Code (stable + insiders) — Application Support holds workspace state,
  # extensions, signed-in account tokens for GitHub/Microsoft, log files,
  # cookies, and the entire user settings tree. ~/.vscode holds extensions
  # installed by the user.
  clear_path "$home/Library/Application Support/Code"
  clear_path "$home/Library/Application Support/Code - Insiders"
  clear_path "$home/Library/Application Support/VSCodium"
  clear_path "$home/.vscode"
  clear_path "$home/.vscode-insiders"
  clear_path "$home/.vscode-oss"

  # JetBrains family — IntelliJ IDEA, PyCharm, WebStorm, GoLand, CLion, etc.
  # The toolbox config and per-IDE state both live here. Trial registration
  # tokens, recent projects, GitHub auth all under these paths.
  clear_path "$home/Library/Application Support/JetBrains"
  clear_path "$home/Library/Caches/JetBrains"
  clear_path "$home/Library/Logs/JetBrains"
  clear_path "$home/Library/Preferences/com.jetbrains.toolbox.plist"
  clear_path "$home/.idea"

  # Sublime Text 2/3/4 — same shape, separate dirs.
  clear_path "$home/Library/Application Support/Sublime Text"
  clear_path "$home/Library/Application Support/Sublime Text 2"
  clear_path "$home/Library/Application Support/Sublime Text 3"

  # Atom (sunset but still on some lab machines)
  clear_path "$home/Library/Application Support/Atom"
  clear_path "$home/.atom"

  # ── Dev tooling and source control ─────────────────────────────────────────
  clear_path "$home/Library/Application Support/GitHub Desktop"
  clear_path "$home/Library/Application Support/com.elgato.StreamDeck"
  clear_path "$home/.gitconfig"
  clear_path "$home/.git-credentials"
  clear_path "$home/.ssh/known_hosts"
  # Don't wipe ~/.ssh entirely — admin may have provisioned id_ed25519 etc.

  # Docker Desktop (logged-in Docker Hub account, settings, image cache)
  clear_path "$home/Library/Containers/com.docker.docker"
  clear_path "$home/Library/Group Containers/group.com.docker"
  clear_path "$home/Library/Application Support/Docker Desktop"

  # Postman, Insomnia (API tooling — workspaces and signed-in accounts)
  clear_path "$home/Library/Application Support/Postman"
  clear_path "$home/Library/Application Support/Insomnia"

  # ── Communication / collaboration ──────────────────────────────────────────
  clear_path "$home/Library/Application Support/Slack"
  clear_path "$home/Library/Containers/com.tinyspeck.slackmacgap"
  clear_path "$home/Library/Application Support/discord"
  clear_path "$home/Library/Application Support/discordcanary"
  clear_path "$home/Library/Application Support/zoom.us"
  clear_path "$home/Library/Application Support/us.zoom.xos"
  clear_path "$home/Library/Caches/us.zoom.xos"
  clear_path "$home/Library/Application Support/Webex"
  clear_path "$home/Library/Application Support/Cisco Webex Meetings"

  # ── Productivity ───────────────────────────────────────────────────────────
  clear_path "$home/Library/Application Support/Notion"
  clear_path "$home/Library/Application Support/Obsidian"
  clear_path "$home/Library/Application Support/Figma"
  clear_path "$home/Library/Application Support/Spotify"
  clear_path "$home/Library/Application Support/Dropbox"
  clear_path "$home/Library/Application Support/Box"

  # ── Blanket sandboxed-app wipe ─────────────────────────────────────────────
  # macOS App Store apps and many Apple-bundled apps store their per-user
  # data under ~/Library/Containers/<bundle-id>. This includes Mail.app,
  # Notes, Messages, Reminders, Safari (sandboxed), TextEdit, etc.
  # For a lab, "signed in" state for ANY sandboxed app — including ones we
  # don't know about — lives in this tree, so blanket-wiping it is the
  # one move that catches the long tail of "what about app X" questions.
  #
  # Trade-off: this removes Mail mailboxes, Messages history, Notes, etc.
  # If your lab admin has been signed in to those during baseline prep,
  # that data leaves with the wipe. For a true student-lab setup that's
  # the desired behaviour. To preserve specific containers, opt out by
  # setting NIVENIA_PRESERVE_CONTAINERS to a colon-separated list of
  # bundle IDs (e.g. "com.apple.mail:com.apple.Notes").
  if [[ -d "$home/Library/Containers" ]]; then
    local preserve_pattern=""
    if [[ -n "${NIVENIA_PRESERVE_CONTAINERS:-}" ]]; then
      preserve_pattern="$NIVENIA_PRESERVE_CONTAINERS"
    fi
    while IFS= read -r -d '' container; do
      local cname
      cname="$(basename "$container")"
      local skip=0
      if [[ -n "$preserve_pattern" ]]; then
        IFS=':' read -ra _PRESERVE <<< "$preserve_pattern"
        for keep in "${_PRESERVE[@]}"; do
          [[ "$cname" == "$keep" ]] && skip=1 && break
        done
      fi
      [[ "$skip" == "1" ]] && continue
      rm -rf "$container" >/dev/null 2>&1 || true
    done < <(find "$home/Library/Containers" -mindepth 1 -maxdepth 1 -print0 2>/dev/null || true)
  fi

  # Group Containers — shared between sister apps (Office suite,
  # Microsoft auth helpers, etc.). Same blanket wipe rationale.
  clear_dir_contents "$home/Library/Group Containers"

  # HTTP cookies / modern HTTPStorages (used by WKWebView-based apps,
  # i.e. most Electron and native macOS apps that embed web views).
  clear_dir_contents "$home/Library/HTTPStorages"
  clear_dir_contents "$home/Library/Cookies"

  ensure_owned_dir "$user" "$home/Library/Caches"
  clear_dir_contents "$home/Library/Caches"
}

if [[ "$MODE" != "preflight" ]]; then
  log "stopping browsers and user cache writers"
  kill_if_running "Safari"
  kill_if_running "Google Chrome"
  kill_if_running "Microsoft Edge"
  kill_if_running "firefox"
  kill_if_running "Teams"
  kill_if_running "Microsoft Teams"
  kill_if_running "Adobe Desktop Service"
  kill_if_running "Creative Cloud"
  kill_if_running "OneDrive"
  kill_if_running "Microsoft Excel"
  kill_if_running "Microsoft Word"
  kill_if_running "Microsoft PowerPoint"
  kill_if_running "Microsoft Outlook"
  kill_if_running "Android Studio"
  kill_if_running "Azure Data Studio"
  kill_if_running "Blender"
  kill_if_running "PacketTracer"
  # Editors — Electron apps re-create their state on quit if running.
  kill_if_running "Code"
  kill_if_running "Code - Insiders"
  kill_if_running "VSCodium"
  kill_if_running "Sublime Text"
  kill_if_running "Atom"
  # JetBrains IDEs are launched via per-IDE binaries; toolbox manager too.
  kill_if_running "idea"
  kill_if_running "pycharm"
  kill_if_running "webstorm"
  kill_if_running "goland"
  kill_if_running "clion"
  kill_if_running "JetBrains Toolbox"
  # Communication and dev tooling
  kill_if_running "Slack"
  kill_if_running "Discord"
  kill_if_running "DiscordCanary"
  kill_if_running "zoom.us"
  kill_if_running "Postman"
  kill_if_running "Insomnia"
  kill_if_running "Notion"
  kill_if_running "Obsidian"
  kill_if_running "Figma"
  kill_if_running "Spotify"
  kill_if_running "Docker Desktop"
  kill_if_running "GitHub Desktop"
  # cfprefsd holds plist file handles open and reads on demand; killing it
  # forces preference flushes so subsequent rm -rf doesn't race a write.
  kill_if_running "cfprefsd"
fi

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

log "sanitizing system caches (preserving desktop wallpaper cache)"
# Wipe /Library/Caches contents but keep the desktop-wallpaper cache so a
# custom wallpaper applied before freeze survives into the baseline and is
# restored after user changes.
if [[ -d "/Library/Caches" ]]; then
  find "/Library/Caches" -mindepth 1 -maxdepth 1 \
    ! -name "com.apple.loginwindow" \
    ! -name "Desktop Pictures" \
    ! -name "com.apple.desktop.admin.png" \
    -exec rm -rf {} + >/dev/null 2>&1 || true
fi
chown root:wheel "/Library/Caches" >/dev/null 2>&1 || true
chmod 755 "/Library/Caches" >/dev/null 2>&1 || true

log "pre-capture cleanup complete"
