#!/usr/bin/env bash
set -euo pipefail

SUBCMD="${1:-}"
if [[ -n "$SUBCMD" ]]; then
  shift
fi

TARGET_ROOT="${1:-}"
SNAPSHOT_NAME="${2:-${NIVENIA_SNAPSHOT_NAME:-}}"

usage() {
  echo "usage: $0 <disable|revert> [volume] [snapshot-name]" >&2
  echo "example: $0 revert /Volumes/\"Macintosh HD - Data\"" >&2
  echo "example: $0 revert /Volumes/\"Macintosh HD - Data\" nivenia-baseline" >&2
}

list_candidate_volumes() {
  local candidates=()
  local vol
  for vol in /Volumes/*; do
    [[ -d "$vol" ]] || continue
    local base
    base="$(basename "$vol")"
    case "$base" in
      Preboot|VM|Recovery|Update|iSCPreboot|xART)
        continue
        ;;
    esac
    candidates+=("$vol")
  done
  printf '%s\n' "${candidates[@]}"
}

auto_detect_volume() {
  if [[ -n "${NIVENIA_RECOVERY_VOLUME:-}" ]]; then
    printf '%s' "$NIVENIA_RECOVERY_VOLUME"
    return 0
  fi

  local candidates
  IFS=$'\n' read -r -d '' -a candidates < <(list_candidate_volumes && printf '\0') || true

  local vol base lower
  for vol in "${candidates[@]}"; do
    base="$(basename "$vol")"
    if [[ "$base" == *" - Data"* || "$base" == *"Data"* ]]; then
      printf '%s' "$vol"
      return 0
    fi
  done

  for vol in "${candidates[@]}"; do
    base="$(basename "$vol")"
    lower="$(printf '%s' "$base" | tr '[:upper:]' '[:lower:]')"
    if [[ "$lower" == *"macos"* || "$lower" == *"macintosh"* ]]; then
      printf '%s' "$vol"
      return 0
    fi
  done

  if [[ ${#candidates[@]} -eq 1 ]]; then
    printf '%s' "${candidates[0]}"
    return 0
  fi

  if [[ ${#candidates[@]} -gt 1 ]]; then
    echo "multiple volumes found; specify one explicitly:" >&2
    printf '  %s\n' "${candidates[@]}" >&2
  fi

  return 1
}

resolve_root() {
  if [[ -n "$TARGET_ROOT" ]]; then
    printf '%s' "$TARGET_ROOT"
    return 0
  fi
  auto_detect_volume
}

read_snapshot_state_name() {
  local root="$1"
  local state_path="$root/var/lib/nivenia/snapshot.json"
  local name=""

  if [[ -f "$state_path" ]]; then
    name="$(grep -o '"name"[[:space:]]*:[[:space:]]*"[^"]*"' "$state_path" | head -n 1 | sed -E 's/.*"name"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/')"
  fi

  printf '%s' "$name"
}

disable_restore() {
  local root="$1"
  local lprefix="$root/Library/LaunchDaemons"
  local sprefix="$root/var/lib/nivenia"

  disable_plist() {
    local base="$1"
    local src="$lprefix/$base"
    local dst="$lprefix/$base.disabled"
    if [[ -f "$src" ]]; then
      mv "$src" "$dst"
      echo "disabled $src"
    fi
  }

  mkdir -p "$sprefix"
  cat > "$sprefix/state.json" <<EOF
{"mode":"thawed","last_restore_ok":true,"last_message":"emergency thaw","updated_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
  echo "wrote thawed state at $sprefix/state.json"

  disable_plist "com.nivenia.restore.plist"
  disable_plist "com.nivenia.updater.plist"

  if [[ -z "$root" ]] && command -v launchctl >/dev/null 2>&1; then
    launchctl bootout system /Library/LaunchDaemons/com.nivenia.restore.plist >/dev/null 2>&1 || true
    launchctl bootout system /Library/LaunchDaemons/com.nivenia.updater.plist >/dev/null 2>&1 || true
  fi

  echo "nivenia recovery disable complete"
}

revert_snapshot() {
  local volume="$1"

  if ! command -v diskutil >/dev/null 2>&1; then
    echo "diskutil is not available; recovery shell is required" >&2
    exit 1
  fi

  if [[ -z "$volume" ]]; then
    echo "volume not found; specify the target volume" >&2
    exit 1
  fi

  if [[ -z "$SNAPSHOT_NAME" ]]; then
    SNAPSHOT_NAME="$(read_snapshot_state_name "$volume")"
  fi
  if [[ -z "$SNAPSHOT_NAME" ]]; then
    SNAPSHOT_NAME="nivenia-baseline"
  fi

  echo "volume: $volume"
  echo "snapshot: $SNAPSHOT_NAME"

  if ! diskutil info "$volume" | grep -qi "APFS"; then
    echo "volume is not APFS: $volume" >&2
    exit 1
  fi

  if ! diskutil apfs listSnapshots "$volume" >/dev/null 2>&1; then
    echo "failed to list snapshots for $volume" >&2
    exit 1
  fi

  if diskutil unmount "$volume" >/dev/null 2>&1; then
    echo "unmounted $volume"
  fi

  if diskutil apfs revertToSnapshot "$volume" -name "$SNAPSHOT_NAME"; then
    echo "revert completed"
  else
    echo "revert failed; ensure the volume is not in use and try again" >&2
    exit 1
  fi

  if diskutil mount "$volume" >/dev/null 2>&1; then
    echo "remounted $volume"
  fi
}

case "$SUBCMD" in
  disable)
    root=""
    if detected="$(resolve_root 2>/dev/null || true)"; then
      root="$detected"
    fi
    disable_restore "$root"
    ;;
  revert)
    volume="$(resolve_root || true)"
    if [[ -z "$volume" ]]; then
      usage
      exit 1
    fi
    revert_snapshot "$volume"
    ;;
  *)
    usage
    exit 1
    ;;
esac
