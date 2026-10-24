# BurnBox

BurnBox watches a folder, and trashes any file or folder that's been in it longer than 24 hours. A temp folder that really is temporary.

Expired entries are sent to the system trash:

| OS | Trash mechanism |
| --- | --- |
| Windows | Recycle Bin (`SHFileOperationW` with `FOF_ALLOWUNDO`) |
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

## Development

Build (requires Go 1.26.10 or later):

```sh
go build -o BurnBox .
```

Run the tests:

```sh
go test ./...
```
