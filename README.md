# Nivenia

A reboot-to-restore system for Intel Mac lab environments. On every boot, Nivenia restores a set of directories from a frozen APFS snapshot back to the live volume. Changes made during a session — downloaded files, installed applications, browser data, login credentials — are wiped on the next reboot.

---

## Requirements

- Intel Mac (x86\_64 only)
- macOS Monterey (12) through Sequoia (15)
- Go 1.22 or later (build from source only)
- Root / sudo access during setup

---

## How it works

1. **Setup** cleans up user session data, then captures an APFS snapshot of the Data volume via `tmutil localsnapshot`.
2. **On every boot**, a LaunchDaemon mounts that snapshot read-only (`mount_apfs -o nobrowse`) and rsyncs the configured restore paths back to the live volume — deleting anything not in the snapshot.
3. The snapshot is the permanent baseline. Changes only affect the live volume and are reversed at the next reboot.

What gets restored is controlled by `restore_paths` in `policy.json`. Defaults are `/System/Volumes/Data/Users` and `/System/Volumes/Data/Applications`.

---

## Quick start

```sh
git clone https://github.com/kpawnd/Nivenia.git
cd Nivenia
bash scripts/setup.sh
```

Setup requires sudo and an internet-connected Go toolchain. It will prompt for your password.

### What setup does

1. Builds `niveniad` and `niveniactl` from source
2. Installs binaries, scripts, and policy to system paths
3. Configures log rotation under `/etc/newsyslog.d/`
4. Runs a preflight check on user directory ownership
5. Clears user session and cache data (see below)
6. Captures the baseline APFS snapshot and sets frozen mode
7. Verifies the restore works before registering launch daemons
8. Registers and starts the restore and updater daemons

### What the cleanup clears

Before capturing the baseline, setup wipes:

- `~/Downloads`, `~/Documents`
- Safari, Chrome, Edge, Firefox: cookies, history, sessions, login data
- Microsoft Teams, Microsoft 365 (Word, Excel, Outlook, OneDrive)
- Adobe Creative Cloud, Blender, Azure Data Studio, Android Studio, Cisco Packet Tracer
- `~/Library/Caches`
- `/Library/Caches` — **except** `com.apple.loginwindow/`, `Desktop Pictures/`, and `com.apple.desktop.admin.png` (preserves any custom wallpaper applied via script before freeze)

It does **not** touch `/etc/sudoers`, system preferences, or installed applications.

---

## Commands

All state-changing commands require `sudo`.

| Command | Effect |
|---|---|
| `sudo niveniactl status` | Show current mode and last restore result |
| `sudo niveniactl --policy /etc/nivenia/policy.json freeze` | Capture a new baseline and set mode to frozen |
| `sudo niveniactl thaw` | Skip all restores until manually refrozen |
| `sudo niveniactl thaw-once` | Skip the next boot restore only, then return to frozen |
| `sudo nivenia-update` | Check for and apply an update |
| `sudo nivenia-recovery disable` | Emergency: force thawed mode and disable daemons |
| `sudo nivenia-recovery revert` | Emergency: manually rsync the snapshot onto the live volume |
| `sudo nivenia-prepare-clean-capture` | Re-run session cleanup without a full setup |

### Mode behaviour

| Mode | Behaviour |
|---|---|
| `frozen` | Restores baseline on every boot |
| `thaw` | No restore until refrozen |
| `thaw_once` | Skips one boot restore, then reverts to frozen |

> **Important — refreezing:** Always run `sudo nivenia-prepare-clean-capture` before `freeze` to avoid capturing a dirty baseline (browser cookies, student files, etc.).

---

## Configuration

Default policy: `/etc/nivenia/policy.json`

```json
{
  "managed_root": "/System/Volumes/Data",
  "restore_paths": [
    "/System/Volumes/Data/Users",
    "/System/Volumes/Data/Applications"
  ],
  "state_file": "/var/lib/nivenia/state.json",
  "log_file": "/var/log/nivenia.log"
}
```

| Field | Description |
|---|---|
| `managed_root` | The APFS Data volume root. Snapshot is taken here. |
| `restore_paths` | Directories rsynced from snapshot to live on each boot |
| `state_file` | Path to the runtime state JSON |
| `log_file` | Path to the main log file |

---

## Logs

| File | Contents |
|---|---|
| `/var/log/nivenia.log` | Restore events (started, completed, skipped, failed) |
| `/var/log/niveniad.err.log` | Daemon stderr — detailed restore progress and errors |
| `/var/log/niveniad.out.log` | Daemon stdout |
| `/var/log/nivenia-updater.log` | Updater run log |

Logs rotate at 5 MB, keeping 7 files.

---

## Updater

Nivenia checks for updates every 6 hours via a LaunchDaemon. It downloads the latest release from GitHub, replaces binaries in place, and restarts only the updater daemon. The restore daemon picks up new binaries on the next natural reboot — it is never restarted mid-session.

```sh
sudo nivenia-update   # manual check
```

Updater log: `/var/log/nivenia-updater.log`

---

## Scheduled restart

Nivenia does **not** ship its own restart scheduler. Scheduled power-on and power-off are handled by macOS `pmset` (configured out-of-band for each lab), and the boot restore fires automatically on the next startup.

---

## Recovery mode

Use this if the machine gets stuck during startup.

**1.** Boot into macOS Recovery (hold Power on Apple Silicon or Cmd+R on Intel at startup) and open Terminal.

**2.** Mount the Data volume:

```sh
diskutil list
diskutil mount "Macintosh HD - Data"
```

**3.** Disable Nivenia (forces thawed mode, disables daemons):

```sh
sudo bash "/Volumes/Macintosh HD - Data/var/lib/nivenia/recovery/nivenia-recovery.sh" disable
```

**4.** Or revert the snapshot manually (rsyncs baseline back to live):

```sh
sudo bash "/Volumes/Macintosh HD - Data/var/lib/nivenia/recovery/nivenia-recovery.sh" revert
```

The recovery script auto-detects the volume. Pass the path explicitly if it fails:

```sh
sudo bash "/Volumes/Macintosh HD - Data/var/lib/nivenia/recovery/nivenia-recovery.sh" disable "/Volumes/Macintosh HD - Data"
```

---

## Test matrix

Community-tested configurations. Add a row or fill in a result by opening a PR against `dev`.

| macOS | Version | Tested | Notes |
|---|---|---|---|
| Monterey | 12 | ☐ | |
| Ventura | 13 | ☐ | |
| Sonoma | 14 | ✓ | Boot restore verified. Desktop, /Applications, files up to 50 GB. |
| Sequoia | 15 | ✓ | Boot restore verified. Directories created in Desktop/Documents were purged on next boot. Applications installed into `/Applications` by a non-admin user were also purged. Requires Full Disk Access to be granted to `niveniad` in System Settings → Privacy & Security. |

---

## Releases and versioning

### Creating a release

Releases are published automatically by the GitHub Actions workflow when you push a version tag:

```sh
git tag v1.0.0
git push origin v1.0.0
```

The workflow builds the `darwin/amd64` binary, packages it into a `.tar.gz`, and publishes a GitHub Release. The updater on installed machines picks it up automatically within 6 hours.

### Dev builds

Pushes to `main` or `dev` without a tag produce a dev build versioned as `dev-<sha>`. These are published as pre-releases and are not picked up by the auto-updater (which only tracks the latest full release).

### Version format

| Trigger | Example version |
|---|---|
| `git tag v1.2.3` | `1.2.3` |
| Push to `main`/`dev` | `dev-a3f1c2e` |

---

## Branch strategy

| Branch | Purpose |
|---|---|
| `main` | Stable, tagged releases only |
| `dev` | Active development — clone this branch for testing |

To test the latest code before it is tagged:

```sh
git clone -b dev https://github.com/kpawnd/Nivenia.git
cd Nivenia
bash scripts/setup.sh
```

---

## File layout

```
/usr/local/libexec/niveniad                        restore daemon
/usr/local/libexec/nivenia-updater                 updater script
/usr/local/libexec/nivenia-prepare-clean-capture   pre-capture cleanup script
/usr/local/bin/niveniactl                          CLI control tool
/usr/local/bin/nivenia-update                      updater alias
/usr/local/bin/nivenia-recovery                    recovery tool
/etc/nivenia/policy.json                           policy
/var/lib/nivenia/state.json                        runtime mode state
/var/lib/nivenia/snapshot.json                     snapshot name and volume
/var/lib/nivenia/recovery/                         recovery scripts (accessible from Recovery OS)
/Library/LaunchDaemons/com.nivenia.restore.plist
/Library/LaunchDaemons/com.nivenia.updater.plist
```

---

## License

MIT License. Keep copyright and license notices when redistributing.
