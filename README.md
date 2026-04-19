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
- installs policy to `/etc/nivenia/policy.json`
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
```

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

- `managed_root`: system-wide writable root to protect
- `baseline_root`: snapshot storage location
- `exclude_paths`: always excluded from baseline/restore

## Release pipeline

Workflow: `.github/workflows/release.yml`

- builds Intel macOS binaries
- packages `niveniad`, `niveniactl`, `policy.json`, `setup.sh`, `update.sh`, and launchd plists into a release tarball
- publishes on `v*` tags

## Notes

- first `freeze` captures baseline and can take time
- baseline capture and restore use `rsync`
- state is stored at `/var/lib/nivenia/state.json`

## License

MIT License. Keep copyright and license notices when redistributing.
