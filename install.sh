#!/bin/sh
# protonfs installer
#
#   curl -fsSL https://raw.githubusercontent.com/radonc0de/protonfs/main/install.sh | sh
#
# Installs (all under your home directory, no root needed):
#   ~/.local/bin/protonfs                      the filesystem
#   ~/.local/share/protonfs/protonfs-bridge.mjs  Proton Drive SDK bridge
#   ~/.local/share/protonfs/node/              Node.js, only if yours is missing or < 20
#   ~/.local/bin/proton-drive                  Proton's official CLI (used for sign-in), if missing
# then signs you in, asks where to mount, and enables a systemd user service.
#
# Non-interactive: PROTONFS_MOUNT=~/ProtonDrive PROTONFS_CACHE_SIZE=10G PROTONFS_LINGER=0 sh install.sh
# Other knobs: PROTONFS_VERSION=v0.1.0 (default: latest release)
#              PROTONFS_FROM=/path/to/dir  (install protonfs-linux-<arch> + protonfs-bridge.mjs from a local dir)
set -eu

REPO="radonc0de/protonfs"
VERSION="${PROTONFS_VERSION:-latest}"
NODE_VERSION="v22.12.0"
BIN_DIR="$HOME/.local/bin"
DATA_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/protonfs"
CONF_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/protonfs"
ORIG_PATH="$PATH"

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

TTY=""
if [ -r /dev/tty ] && (exec </dev/tty) 2>/dev/null; then TTY=/dev/tty; fi

# ask VAR "Question" "default"
ask() {
    _var=$1 _q=$2 _def=$3
    eval "_cur=\${$_var:-}"
    if [ -n "$_cur" ]; then return; fi
    if [ -z "$TTY" ]; then eval "$_var=\$_def"; return; fi
    printf '%s [%s]: ' "$_q" "$_def" >/dev/tty
    IFS= read -r _ans </dev/tty || _ans=""
    [ -n "$_ans" ] || _ans=$_def
    eval "$_var=\$_ans"
}

fetch() { # fetch URL OUTFILE
    if command -v curl >/dev/null 2>&1; then curl -fsSL --retry 3 -o "$2" "$1"
    elif command -v wget >/dev/null 2>&1; then wget -q -O "$2" "$1"
    else die "need curl or wget"; fi
}

sha256_check() { # sha256_check FILE EXPECTED
    _got=$(sha256sum "$1" | cut -d' ' -f1)
    [ "$_got" = "$2" ] || die "checksum mismatch for $1"
}

# ---------- preflight ----------
bold "protonfs installer"
[ "$(uname -s)" = Linux ] || die "protonfs supports Linux only"
case "$(uname -m)" in
    x86_64|amd64) GOARCH=amd64; NODEARCH=x64; PDARCH=x64 ;;
    aarch64|arm64) GOARCH=arm64; NODEARCH=arm64; PDARCH=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
esac
if ldd --version 2>&1 | grep -qi musl; then PDARCH="$PDARCH-musl"; MUSL=1; else MUSL=0; fi

missing=""
command -v fusermount3 >/dev/null 2>&1 || command -v fusermount >/dev/null 2>&1 || missing="$missing fuse3"
[ -e /dev/fuse ] || missing="$missing /dev/fuse(kernel-module)"
command -v secret-tool >/dev/null 2>&1 || missing="$missing secret-tool"
command -v systemctl >/dev/null 2>&1 || missing="$missing systemd"
command -v tar >/dev/null 2>&1 || missing="$missing tar"
if [ -n "$missing" ]; then
    printf 'Missing:%s\n\n' "$missing" >&2
    cat >&2 <<'EOF'
Install them with your package manager, e.g.:
  Fedora/RHEL:   sudo dnf install fuse3 libsecret
  Debian/Ubuntu: sudo apt install fuse3 libsecret-tools
  Arch:          sudo pacman -S fuse3 libsecret
protonfs also needs a running keyring (GNOME Keyring or KWallet), as on any desktop session.
EOF
    exit 1
fi
systemctl --user show-environment >/dev/null 2>&1 || die "systemd user session not available (log in to a normal desktop/SSH session)"

mkdir -p "$BIN_DIR" "$DATA_DIR" "$CONF_DIR"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---------- Node.js ----------
NODE=""
if command -v node >/dev/null 2>&1; then
    major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)
    if [ "$major" -ge 20 ]; then NODE=$(command -v node); fi
fi
if [ -z "$NODE" ] && [ -x "$DATA_DIR/node/bin/node" ]; then NODE="$DATA_DIR/node/bin/node"; fi
if [ -z "$NODE" ]; then
    [ "$MUSL" = 0 ] || die "Node.js >= 20 is required; install it with your package manager (e.g. apk add nodejs)"
    bold "Installing Node.js $NODE_VERSION (private copy for protonfs)"
    base="https://nodejs.org/dist/$NODE_VERSION"
    file="node-$NODE_VERSION-linux-$NODEARCH.tar.xz"
    fetch "$base/SHASUMS256.txt" "$TMP/SHASUMS256.txt"
    fetch "$base/$file" "$TMP/$file"
    sha256_check "$TMP/$file" "$(grep " $file\$" "$TMP/SHASUMS256.txt" | cut -d' ' -f1)"
    rm -rf "$DATA_DIR/node"
    mkdir -p "$DATA_DIR/node"
    tar -xJf "$TMP/$file" -C "$DATA_DIR/node" --strip-components=1
    NODE="$DATA_DIR/node/bin/node"
fi
info "node: $NODE ($("$NODE" --version))"

# ---------- protonfs ----------
bold "Installing protonfs"
if [ -n "${PROTONFS_FROM:-}" ]; then
    cp "$PROTONFS_FROM/protonfs-linux-$GOARCH" "$TMP/protonfs"
    cp "$PROTONFS_FROM/protonfs-bridge.mjs" "$TMP/protonfs-bridge.mjs"
else
    if [ "$VERSION" = latest ]; then rel="https://github.com/$REPO/releases/latest/download"
    else rel="https://github.com/$REPO/releases/download/$VERSION"; fi
    fetch "$rel/SHA256SUMS" "$TMP/SHA256SUMS"
    fetch "$rel/protonfs-linux-$GOARCH" "$TMP/protonfs"
    fetch "$rel/protonfs-bridge.mjs" "$TMP/protonfs-bridge.mjs"
    sha256_check "$TMP/protonfs" "$(grep " protonfs-linux-$GOARCH\$" "$TMP/SHA256SUMS" | cut -d' ' -f1)"
    sha256_check "$TMP/protonfs-bridge.mjs" "$(grep " protonfs-bridge.mjs\$" "$TMP/SHA256SUMS" | cut -d' ' -f1)"
fi
install -m 755 "$TMP/protonfs" "$BIN_DIR/protonfs"
install -m 644 "$TMP/protonfs-bridge.mjs" "$DATA_DIR/protonfs-bridge.mjs"
info "$("$BIN_DIR/protonfs" version) -> $BIN_DIR/protonfs"

# ---------- Proton's CLI (for sign-in) ----------
PD=$(command -v proton-drive 2>/dev/null || true)
[ -n "$PD" ] || { [ -x "$BIN_DIR/proton-drive" ] && PD="$BIN_DIR/proton-drive"; } || true
if [ -z "$PD" ]; then
    bold "Installing Proton Drive CLI (official, from proton.me)"
    fetch "https://proton.me/download/drive/cli/version.json" "$TMP/version.json"
    # prints "URL SHA512" for our platform from Proton's release manifest
    pdinfo=$("$NODE" -e '
        const m = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8"));
        const rel = m.Releases.find(r => r.CategoryName === "Stable") || m.Releases[0];
        const f = rel.Files.find(f => f.Platform === "linux/" + process.argv[2]);
        if (!f) process.exit(1);
        console.log(f.Url, f.Sha512CheckSum);
    ' "$TMP/version.json" "$PDARCH") || die "no Proton Drive CLI build for linux/$PDARCH"
    # shellcheck disable=SC2086  # split "URL SHA512" into $1 $2
    set -- $pdinfo
    fetch "$1" "$TMP/proton-drive"
    [ "$(sha512sum "$TMP/proton-drive" | cut -d' ' -f1)" = "$2" ] || die "checksum mismatch for proton-drive"
    install -m 755 "$TMP/proton-drive" "$BIN_DIR/proton-drive"
    PD="$BIN_DIR/proton-drive"
fi
info "proton-drive: $PD"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) export PATH="$BIN_DIR:$PATH" ;; esac

# ---------- sign in ----------
bold "Proton sign-in"
if "$PD" filesystem list / >/dev/null 2>&1; then
    info "already signed in"
else
    [ -n "$TTY" ] || die "not signed in; run: $PD auth login   (then re-run this installer)"
    info "A browser window will open (or a link is shown) to sign in to Proton."
    "$PD" auth login </dev/tty || die "sign-in failed"
    "$PD" filesystem list / >/dev/null 2>&1 || die "sign-in did not complete"
fi

# ---------- where & how ----------
bold "Mount location"
ask PROTONFS_MOUNT "Directory to mount Proton Drive at (must be yours; created if missing)" "$HOME/ProtonDrive"
case "$PROTONFS_MOUNT" in "~"*) PROTONFS_MOUNT="$HOME${PROTONFS_MOUNT#\~}" ;; esac
ask PROTONFS_CACHE_SIZE "Max local cache size (least-recently-used files are evicted)" "10G"
ask PROTONFS_LINGER "Keep it mounted after you log out? (y/N)" "N"

if [ -f "$CONF_DIR/config.env" ] && grep -q '^PROTONFS_CACHE_SIZE=' "$CONF_DIR/config.env"; then
    sed -i "s|^PROTONFS_CACHE_SIZE=.*|PROTONFS_CACHE_SIZE=$PROTONFS_CACHE_SIZE|" "$CONF_DIR/config.env"
else
    printf 'PROTONFS_CACHE_SIZE=%s\n' "$PROTONFS_CACHE_SIZE" >>"$CONF_DIR/config.env"
fi
grep -q '^PROTONFS_NODE=' "$CONF_DIR/config.env" || printf 'PROTONFS_NODE=%s\n' "$NODE" >>"$CONF_DIR/config.env"

linger=""
case "$PROTONFS_LINGER" in y|Y|yes|YES|1|true) linger="--linger" ;; esac

bold "Enabling service"
# shellcheck disable=SC2086  # $linger is empty or one flag
"$BIN_DIR/protonfs" enable "$PROTONFS_MOUNT" $linger

echo
bold "Done."
info "Proton Drive:   $PROTONFS_MOUNT/my-files"
info "Status/logs:    protonfs status   |   journalctl --user -u 'protonfs@*' -f"
info "Remove mount:   protonfs disable \"$PROTONFS_MOUNT\""
info "Settings:       $CONF_DIR/config.env"
case ":$ORIG_PATH:" in *":$BIN_DIR:"*) ;; *) info "Add $BIN_DIR to your PATH to use the protonfs command." ;; esac
