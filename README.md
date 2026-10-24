# BurnBox

BurnBox watches a folder, and trashes any file or folder that's been in it longer than 24 hours. A temp folder that really is temporary.

Expired entries are sent to the system trash:

| OS | Trash mechanism |
| --- | --- |
| Windows | Recycle Bin |
| macOS | Trash (Finder via AppleScript) |
| Linux | freedesktop.org Trash (`~/.local/share/Trash` or `$XDG_DATA_HOME/Trash`) |

## Usage

```sh
BurnBox [flags]
```

| Flag | Default | Description |
| --- | --- | --- |
| `--folder` | `~/BurnBox` | Folder path to watch (top-level only). Created if missing. |
| `--expiration` | `24h` | Duration measured from when BurnBox first saw the entry (e.g. `30m`, `24h`). |
| `--ignore` | `.directory` | Comma-separated list of filenames to never touch. |
| `--force` | `false` | Delete instead of sending to the trash. |
| `--store` | `<user cache dir>/BurnBox/store-<folder>-<hash>.json` | Path of the first-seen store. |

Example:

```sh
BurnBox --folder /tmp/drop --expiration 30m
```

## Run at startup

### Windows

Scheduled task at logon:

```sh
schtasks /Create /TN "BurnBox" /SC ONLOGON /TR "C:\Tools\BurnBox\BurnBox.exe --folder C:\Users\you\BurnBox" /RL LIMITED /F
```

Or place a shortcut in the Startup folder (`shell:startup`). Use `/RL HIGHEST` if elevated access is needed; remove with `schtasks /Delete /TN "BurnBox" /F`.

### macOS

Create `~/Library/LaunchAgents/com.lixquid.burnbox.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.lixquid.burnbox</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/BurnBox</string>
        <string>--folder</string>
        <string>/Users/you/BurnBox</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
```

Load (and unload):

```sh
launchctl load ~/Library/LaunchAgents/com.lixquid.burnbox.plist
launchctl unload ~/Library/LaunchAgents/com.lixquid.burnbox.plist
```

This starts at login. For boot-time startup before any user logs in, use `/Library/LaunchDaemons` instead (root-owned, mode `644`).

### Linux (systemd)

Create `~/.config/systemd/user/burnbox.service`:

```ini
[Unit]
Description=BurnBox

[Service]
ExecStart=%h/bin/BurnBox --folder %h/BurnBox
Restart=on-failure

[Install]
WantedBy=default.target
```

Enable and start:

```sh
systemctl --user daemon-reload
systemctl --user enable --now burnbox.service
```

`%h` is the home directory. To keep the service running after logout, enable lingering: `loginctl enable-linger $USER`. For a system-wide service, put the unit in `/etc/systemd/system/` with an absolute `ExecStart` path and a `User=` line.

## Development

Build (requires Go 1.26.10 or later):

```sh
go build -o BurnBox .
```

Run the tests:

```sh
go test ./...
```

## License

MIT — see [LICENSE](LICENSE).
