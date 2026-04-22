#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then
  echo "run as root (use sudo)" >&2
  exit 1
fi

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
{"mode":"thawed","last_restore_ok":true,"last_message":"emergency thaw","updated_at_utc":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
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
    echo "diskutil is not available" >&2
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

  # Get device identifier (/dev/diskXsY) — required by mount_apfs
  local device
  device="$(diskutil info "$volume" | awk '/Device Identifier:/ {print $NF}')"
  if [[ -z "$device" ]]; then
    echo "could not determine device identifier for $volume" >&2
    exit 1
  fi
  device="/dev/$device"
  echo "device: $device"

  # Verify the snapshot exists
  if ! diskutil apfs listSnapshots "$volume" 2>/dev/null | grep -qF "$SNAPSHOT_NAME"; then
    echo "snapshot '$SNAPSHOT_NAME' not found on $volume" >&2
    exit 1
  fi

  # Determine which subdirectories to restore.
  # Reads restore_paths from the installed policy.json if available,
  # otherwise defaults to Users (sufficient for a standard lab reset).
  local restore_subdirs=("Users")
  local policy_path="$volume/private/etc/nivenia/policy.json"
  [[ -f "$policy_path" ]] || policy_path="$volume/etc/nivenia/policy.json"
  if [[ -f "$policy_path" ]] && command -v python3 >/dev/null 2>&1; then
    local configured_root
    configured_root="$(python3 -c "import json; p=json.load(open('$policy_path')); print(p.get('managed_root','').rstrip('/'))" 2>/dev/null || true)"
    if [[ -n "$configured_root" ]]; then
      local abs_paths=()
      mapfile -t abs_paths < <(python3 -c "import json; p=json.load(open('$policy_path')); [print(x) for x in p.get('restore_paths',[])]" 2>/dev/null || true)
      if [[ ${#abs_paths[@]} -gt 0 ]]; then
        restore_subdirs=()
        for abs in "${abs_paths[@]}"; do
          local rel="${abs#"${configured_root}/"}"
          [[ "$rel" != "$abs" && -n "$rel" ]] && restore_subdirs+=("$rel")
        done
      fi
    fi
  fi

  # Mount the snapshot read-only using mount_apfs (available on all macOS with APFS)
  local mount_point
  mount_point="$(mktemp -d)"
  _cleanup_snap_mount() {
    diskutil unmount "$mount_point" >/dev/null 2>&1 || true
    rmdir "$mount_point" >/dev/null 2>&1 || true
  }
  trap _cleanup_snap_mount EXIT

  echo "mounting snapshot..."
  if ! mount_apfs -s "$SNAPSHOT_NAME" "$device" "$mount_point"; then
    echo "failed to mount snapshot $SNAPSHOT_NAME on $device" >&2
    exit 1
  fi
  echo "mounted at $mount_point"

  # Rsync each restore subdir from the frozen snapshot back to the live volume
  local ok=1
  for subdir in "${restore_subdirs[@]}"; do
    local src="$mount_point/$subdir/"
    local dst="$volume/$subdir/"
    if [[ ! -e "$mount_point/$subdir" ]]; then
      echo "WARNING: $subdir not found in snapshot, skipping" >&2
      continue
    fi
    echo "restoring $subdir..."
    if rsync -aH --delete --force "$src" "$dst"; then
      echo "restored $subdir"
    else
      local rc=$?
      if [[ $rc -eq 23 ]]; then
        echo "restored $subdir (some hard links could not be replicated, non-fatal)"
      elif [[ $rc -eq 24 ]]; then
        echo "restored $subdir (some files vanished mid-transfer, non-fatal)"
      else
        echo "rsync failed for $subdir (exit $rc)" >&2
        ok=0
      fi
    fi
  done

  if [[ "$ok" != "1" ]]; then
    echo "revert completed with errors" >&2
    exit 1
  fi

  echo "revert completed"
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
