# protonfs

[![ci](https://github.com/radonc0de/protonfs/actions/workflows/ci.yml/badge.svg)](https://github.com/radonc0de/protonfs/actions/workflows/ci.yml)
[![license: GPL v3](https://img.shields.io/badge/license-GPLv3-blue.svg)](LICENSE)

Mount [Proton Drive](https://proton.me/drive) as a normal Linux filesystem, in any directory you own, automatically at every login.

```sh
curl -fsSL https://raw.githubusercontent.com/radonc0de/protonfs/main/install.sh | sh
```

The installer signs you in to Proton, asks where to mount, and sets up a systemd user service. After that `ls`, `cp`, `rsync`, `mv`, `rm`, editors and file managers all work on your Drive:

```
~/ProtonDrive/
├── my-files/         your Drive
├── devices/          computers synced with the Proton Drive desktop app
└── shared-with-me/   items others shared with you
```

## How it works

```
 kernel FUSE ──► protonfs (Go)  ──JSON over stdio──►  protonfs-bridge (Node.js)
                 │ metadata cache                     │ official Proton Drive SDK
                 │ LRU file cache                     │ end-to-end encryption
                 ▼                                    ▼
          ~/.cache/protonfs/...   ◄── downloads ──  drive-api.proton.me
```

- **protonfs** (Go, [go-fuse](https://github.com/hanwen/go-fuse)) is the filesystem: directory cache, on-disk LRU cache, read/write handling, uploads, systemd integration.
- **protonfs-bridge** (TypeScript) uses Proton's official [`@protontech/drive-sdk`](https://www.npmjs.com/package/@protontech/drive-sdk) for everything that touches Proton: encryption, listing, transfers. File contents never pass between the two processes; the bridge reads and writes the cache files directly.
- **Sign-in** goes through Proton's official [`proton-drive` CLI](https://proton.me/support/drive-cli) (`proton-drive auth login`, which opens a browser). protonfs reuses that session from your system keyring, so it never sees your password. The installer fetches the CLI from proton.me and checks its SHA-512.

### Reads and writes

- **Reads stream.** Opening a file starts a background download into the cache, and reads return as soon as the bytes they need have arrived. Cached files are keyed by revision, so they stay valid across remounts and reboots.
- **Writes are local, then uploaded.** Changes go to a local copy. About a second after the last writer closes the file, it is uploaded as a new file or a new revision; `fsync` uploads immediately and waits. On stop or logout protonfs finishes pending uploads before exiting.
- **The cache is bounded.** It holds at most `PROTONFS_CACHE_SIZE` (default 10G), evicting least-recently-used files first. Unsaved changes are never evicted.
- **Deleting moves items to Proton's trash** unless `PROTONFS_DELETE=permanent`.

## Requirements

- Linux (x86_64 or arm64) with systemd
- `fuse3` and `secret-tool`, plus a running keyring (GNOME Keyring or KWallet), as on any desktop session:
  - Fedora: `sudo dnf install fuse3 libsecret`
  - Debian/Ubuntu: `sudo apt install fuse3 libsecret-tools`
  - Arch: `sudo pacman -S fuse3 libsecret`
- Node.js ≥ 20. If yours is missing or older, the installer downloads a private copy and checks its checksum.

## Usage

```sh
protonfs enable ~/ProtonDrive      # mount now and at every login
protonfs enable ~/Proton --linger  # ...and stay mounted after logout
protonfs status
protonfs disable ~/ProtonDrive
protonfs mount ~/ProtonDrive       # one-off, in the foreground
protonfs login                     # re-sign-in if the session expires
journalctl --user -u 'protonfs@*' -f
```

Several directories can be mounted at once; each one gets its own service (`protonfs@<path>.service`) and its own cache.

Settings go in `~/.config/protonfs/config.env`:

| Variable | Default | |
|---|---|---|
| `PROTONFS_CACHE_SIZE` | `10G` | Maximum local cache size (LRU) |
| `PROTONFS_TTL` | `30` | Seconds to cache directory listings; changes made elsewhere show up after this |
| `PROTONFS_DELETE` | trash | Set to `permanent` to delete instead of trashing |
| `PROTONFS_NODE` | auto | Node.js binary to use |

## Limitations

- **Uploads happen after `close()`, so upload errors are logged rather than returned.** `cp` returns before the upload finishes. Failed uploads are retried; use `fsync` (e.g. `dd conv=fsync`) if you need to know the data is uploaded.
- **Timestamps and permissions aren't stored.** Proton doesn't keep a file's modification time if you set it, so `rsync -a`/`-t` re-copies unchanged files on every run (plain `rsync -r` is fine). chmod and chown are accepted but do nothing.
- **Modifying part of a file re-uploads the whole file** (Proton stores whole-file revisions).
- **Not supported yet:** photos, albums and symlinks.
- **Lingering only works if your keyring is unlocked.** It keeps the mount alive after logout, but after a reboot the mount can't start until you log in once.
- **protonfs is unofficial** and not affiliated with Proton AG.

## Uninstall

```sh
protonfs disable ~/ProtonDrive
rm ~/.local/bin/protonfs ~/.config/systemd/user/protonfs@.service
rm -rf ~/.local/share/protonfs ~/.cache/protonfs ~/.config/protonfs
# optional: proton-drive auth logout && rm ~/.local/bin/proton-drive
```

## Building

```sh
go build -o protonfs ./cmd/protonfs
(cd bridge && npm ci && npm run build)   # -> bridge/dist/protonfs-bridge.mjs
./protonfs mount /some/dir               # finds bridge/dist next to the binary
```

Releases are built by `.github/workflows/release.yml` when a `v*` tag is pushed. See [CONTRIBUTING.md](CONTRIBUTING.md) for development notes; report security issues privately as described in [SECURITY.md](SECURITY.md).

## License

protonfs is free software under the [GNU GPL v3.0 or later](LICENSE). The released bridge bundles Proton's GPL-3.0 [`@protontech/crypto`](https://www.npmjs.com/package/@protontech/crypto); each release includes `THIRD_PARTY_NOTICES.txt` with the licenses of all bundled dependencies.

"Proton" and "Proton Drive" are trademarks of Proton AG. This project is not affiliated with or endorsed by Proton.
