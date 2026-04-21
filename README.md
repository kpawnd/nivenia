# Nivenia reboot-restore

Nivenia provides simple reboot-to-restore for Intel Macs.

Supported macOS versions: Monterey (12) through Sequoia (15) only.

It restores a system-wide writable root (`/System/Volumes/Data`) back to a baseline on every boot while frozen.

## Simple setup

Run this from the repository root:

```sh
bash scripts/setup.sh
```

What setup does:

- builds `niveniad` and `niveniactl`
- installs binaries to `/usr/local/libexec` and `/usr/local/bin`
- installs updater command as `nivenia-update`
- installs recovery command as `nivenia-recovery`
- installs pre-capture cleanup command as `nivenia-prepare-clean-capture`
- installs policy to `/etc/nivenia/policy.json`
- clears browser/session/cache data before first baseline capture (required)
- captures initial baseline
- enables restore and updater launch daemons at boot

## Commands

Use `sudo` for mode changes so macOS asks for the local administrator password.

```sh
sudo niveniactl status
sudo niveniactl thaw
sudo niveniactl thaw-once
sudo niveniactl freeze --policy /etc/nivenia/policy.json --state /var/lib/nivenia/state.json
sudo nivenia-update
sudo nivenia-recovery disable
sudo nivenia-recovery revert
sudo nivenia-prepare-clean-capture
```

Pre-capture cleanup is required. Setup will stop if the cleanup preflight fails.

Cleanup wipes user Downloads, Documents, browser data, and common app caches (Teams, Microsoft 365, Adobe Creative Cloud, Blender, Azure Data Studio, Android Studio, Cisco Packet Tracer). It does not touch `/etc/sudoers`.

Mode behavior:

- `frozen`: restore baseline every boot
- `thaw`: do not restore until frozen again
- `thaw-once`: skip one boot restore, then return to frozen

## Updater service

Nivenia updates itself using a root launchd service:

- plist: `/Library/LaunchDaemons/com.nivenia.updater.plist`
- command: `/usr/local/libexec/nivenia-updater`
- interval: every 21600 seconds (6 hours)
- log: `/var/log/nivenia-updater.log`

Manual update check:

```sh
sudo nivenia-update
```

## Policy

Default policy file: `configs/policy.json`

Key fields:

- `managed_root`: mount point for the data you want reverted on boot
- `NIVENIA_SNAPSHOT_VOLUME` (env): optional override for the APFS volume to snapshot/revert

Notes:

- If `managed_root` points directly at the dedicated APFS volume, you do not need `NIVENIA_SNAPSHOT_VOLUME`.
- Use `NIVENIA_SNAPSHOT_VOLUME` when the managed path is a bind mount or nested mount and the snapshot target is different.

Restore behavior:

- boot restore waits for the configured `managed_root` to be available before restore
- restore daemon runs once at boot (not continuously during uptime)
- restore aborts if an interactive console user is already logged in
- restore uses APFS snapshots (`diskutil apfs snapshot/revertToSnapshot`; on Monterey snapshot creation falls back to `tmutil snapshot`)
- restore verifies integrity before revert (snapshot metadata, policy hash, and installed Nivenia binary/script hashes)
- if integrity verification fails, restore is refused and mode is forced to `thaw`
- restore failures are tracked, but mode is not auto-changed to thawed

## Release pipeline

Workflow: `.github/workflows/release.yml`

- builds Intel macOS binaries
- packages `niveniad`, `niveniactl`, `policy.json`, `setup.sh`, `update.sh`, `nivenia_recovery.sh`, and launchd plists into a release tarball
- publishes on `main` pushes and `v*` tags
- emits normal GitHub Releases so `nivenia-update` can discover them via `/releases/latest`

## Notes

- first `freeze` captures baseline and can take time
- snapshot capture and restore use APFS snapshots (volume-wide)
- state is stored at `/var/lib/nivenia/state.json`
- snapshot name is stored at `/var/lib/nivenia/snapshot.json` (used when snapshot names are auto-generated)
- recovery script is installed at `/var/lib/nivenia/recovery/nivenia-recovery.sh` for Recovery mode use
- restore uses a lock file to prevent concurrent runs (`/var/lib/nivenia/restore.lock`)

Snapshot guidance:

- APFS snapshot revert is volume-wide; you cannot exclude directories the way rsync does.
- For DeepFreeze-like isolation, create a dedicated APFS volume and set `managed_root` to that volume's mount point.
- Optional overrides: set `NIVENIA_SNAPSHOT_VOLUME` if you need a custom volume. `NIVENIA_SNAPSHOT_NAME` is honored when `diskutil apfs snapshot` is available; on Monterey, snapshot names are auto-generated and stored in `/var/lib/nivenia/snapshot.json`.

## Recovery mode

If a system gets stuck during startup:

1. Boot into macOS Recovery and open Terminal.
2. Identify the target volume name:

```sh
diskutil list
```

3. Mount the target data volume (example name below; yours may differ):

```sh
diskutil mount "Macintosh HD - Data"
```

4. Run the recovery script from the mounted volume (it auto-detects if possible):

```sh
bash "/Volumes/Macintosh HD - Data/var/lib/nivenia/recovery/nivenia-recovery.sh" disable "/Volumes/Macintosh HD - Data"
```

This forces thawed mode and disables Nivenia launch daemons.

To revert a snapshot in Recovery (uses `/var/lib/nivenia/snapshot.json` when present):

```sh
bash "/Volumes/Macintosh HD - Data/var/lib/nivenia/recovery/nivenia-recovery.sh" revert "/Volumes/Macintosh HD - Data"
```

If auto-detection fails, set a volume explicitly:

```sh
NIVENIA_RECOVERY_VOLUME="/Volumes/<Your Data Volume>" \
  bash "/Volumes/<Your Data Volume>/var/lib/nivenia/recovery/nivenia-recovery.sh" disable
```

## License

MIT License. Keep copyright and license notices when redistributing.
