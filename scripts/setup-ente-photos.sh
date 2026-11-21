#!/usr/bin/env bash
set -euo pipefail
umask 077

repo=$(cd "$(dirname "$0")/.." && pwd)
data=${XDG_DATA_HOME:-$HOME/.local/share}/lark
config=${XDG_CONFIG_HOME:-$HOME/.config}
case "$*" in
    "") mode=sync ;;
    --full-export) mode=sync-export ;;
    *) printf 'Usage: %s [--full-export]\n' "$0" >&2; exit 2 ;;
esac
version=v0.4.5
case $(uname -m) in
    x86_64)
        arch=amd64
        checksum=4890dc2b27510d0e2f9e7ee5d47bf1f274e145f79d5557aef1d24b3103fadbbe
        binary_checksum=9673e0ce61f8118aa8e5ead819847224752b6efee3910837ede83d4233a90003 ;;
    aarch64)
        arch=arm64
        checksum=721ad071c8a4aba138b6132cf13bbd30800d68ce4f29711d259e492236c2810c
        binary_checksum=fd5098d3207f407d78bada5862f835ff5b24e3649a0e97914278cc56526de6b3 ;;
    *) printf 'Ente Photos setup supports Linux x86_64 and aarch64.\n' >&2; exit 1 ;;
esac
install -d -m 700 -- "$data" "$data/bin" "$data/ente-cli" "$config/lark"
install -d -- "$config/systemd/user"
stage=$(mktemp -d "$data/.ente-install.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
archive=ente-cli-$version-linux-$arch.tar.gz
binary=$data/bin/ente-$version
if [[ ! -x "$binary" || -L "$binary" ]] ||
    ! printf '%s  %s\n' "$binary_checksum" "$binary" | sha256sum --check --status; then
    curl --fail --silent --show-error --location \
        "https://github.com/ente/ente/releases/download/ente-cli-$version/$archive" -o "$stage/$archive"
    printf '%s  %s\n' "$checksum" "$stage/$archive" | sha256sum --check --status
    tar -xzf "$stage/$archive" -C "$stage" -- ente
    printf '%s  %s\n' "$binary_checksum" "$stage/ente" | sha256sum --check --status
    "$stage/ente" --version | grep -Fx "ente ${version#v}"
    install -m 700 -- "$stage/ente" "$binary"
fi
ln -sfn -- "ente-$version" "$data/bin/ente"
install -m 700 -- "$repo/scripts/ente-export-job.sh" "$data/bin/ente-photo-job"
python3 - "$config/systemd/user" "$data" "$config/lark/ente-vault-key" "$mode" <<'PY'
import sys
from pathlib import Path

units, data, key = map(Path, sys.argv[1:4])
mode = sys.argv[4]

def quoted(path):
    return '"' + str(path).translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "$": "$$", "\n": "\\n", "\r": "\\r",
    })) + '"'

(units / "lark-ente-photos.service").write_text(f"""[Unit]
Description=Refresh local Lark Ente Photos metadata

[Service]
Type=oneshot
UMask=0077
Nice=10
IOSchedulingClass=idle
TimeoutStartSec=12h
ExecStart={quoted(data / 'bin/ente-photo-job')} {mode} {quoted(data)} {quoted(key)}
""")
(units / "lark-ente-photos.timer").write_text("""[Unit]
Description=Refresh the local Lark Ente Photos library nightly

[Timer]
OnCalendar=*-*-* 02:30:00
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
""")
PY
printf '%s\n' 'Installed Ente CLI and disabled-by-default user units. See docs/ente-photos.md for login and first sync.'
