# Contributing

Bug reports and pull requests are welcome.

## Reporting bugs

Open an issue with your distro, `protonfs version`, the command that failed, and the log:

```sh
journalctl --user -u 'protonfs@*' --since '10 min ago'
# or run in the foreground with SDK debug output:
PROTONFS_DEBUG=1 protonfs mount /some/dir
```

Logs contain file and folder names from your Drive. Remove anything private before posting. They never contain your password or session tokens; if you ever see one, report it as a security issue (see [SECURITY.md](SECURITY.md)).

## Development

You need Go (see `go.mod`), Node.js 20 or newer, `fuse3` and a signed-in [`proton-drive` CLI](https://proton.me/support/drive-cli).

```sh
go build -o protonfs ./cmd/protonfs
(cd bridge && npm ci && npm run build)
./protonfs mount /tmp/pd        # Ctrl-C to unmount
```

Before you open a PR:

```sh
gofmt -l .        # should print nothing
go vet ./...
go test ./...
shellcheck -s sh -S warning install.sh
```

CI runs the same checks.

Test against a throwaway folder in your Drive, not your real files. Deletes go to Proton's trash by default.

## Layout

| Path | |
|---|---|
| `cmd/protonfs` | CLI: mount, enable/disable (systemd), status, login |
| `internal/pfs` | FUSE filesystem and LRU cache |
| `internal/bridge` | Go client for the Node bridge (JSON lines over stdio) |
| `bridge/src` | Node bridge wrapping `@protontech/drive-sdk` |
| `install.sh` | `curl \| sh` installer |

## Releases

Push a `vX.Y.Z` tag. `.github/workflows/release.yml` builds the binaries and the bridge bundle and publishes a GitHub release with `SHA256SUMS`, which `install.sh` verifies.

## License

By contributing, you agree that your contributions are licensed under the GPL-3.0-or-later license (see [LICENSE](LICENSE)).
