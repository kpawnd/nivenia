#!/usr/bin/env bash
set -euo pipefail

TARGET_ROOT="${1:-}"

resolve_root() {
  if [[ -n "$TARGET_ROOT" ]]; then
    printf '%s' "$TARGET_ROOT"
    return
  fi
  if [[ -d "/Volumes/Macintosh HD - Data" ]]; then
    printf '%s' "/Volumes/Macintosh HD - Data"
    return
  fi
  printf '%s' ""
}

ROOT="$(resolve_root)"
if [[ -z "$ROOT" ]]; then
  ROOT=""
fi

LPREFIX="$ROOT/Library/LaunchDaemons"
SPREFIX="$ROOT/var/lib/nivenia"

disable_plist() {
  local base="$1"
  local src="$LPREFIX/$base"
  local dst="$LPREFIX/$base.disabled"
  if [[ -f "$src" ]]; then
    mv "$src" "$dst"
    echo "disabled $src"
  fi
}

mkdir -p "$SPREFIX"
cat > "$SPREFIX/state.json" <<EOF
{"mode":"thawed","last_restore_ok":true,"last_message":"emergency thaw","updated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
echo "wrote thawed state at $SPREFIX/state.json"

disable_plist "com.nivenia.restore.plist"
disable_plist "com.nivenia.updater.plist"

if [[ -z "$ROOT" ]] && command -v launchctl >/dev/null 2>&1; then
  launchctl bootout system /Library/LaunchDaemons/com.nivenia.restore.plist >/dev/null 2>&1 || true
  launchctl bootout system /Library/LaunchDaemons/com.nivenia.updater.plist >/dev/null 2>&1 || true
fi

echo "nivenia emergency disable complete"
