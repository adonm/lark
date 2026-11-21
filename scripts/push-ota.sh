#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/board.sh
source "$scripts/board.sh"
build=1
host=
for arg in "$@"; do
    if [[ "$arg" == --no-build ]]; then
        build=0
    elif [[ -z "$host" && "$arg" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]]; then
        host=$arg
    else
        host=
        break
    fi
done
if [[ -z "$host" ]]; then
    echo "Usage: $0 [--no-build] <ip_or_host>" >&2
    exit 2
fi

env_file="${XDG_CONFIG_HOME:-$HOME/.config}/echoear/bridge.env"
if [[ -z ${ECHOEAR_BRIDGE_TOKEN:-} && -r "$env_file" ]]; then
    ECHOEAR_BRIDGE_TOKEN=$(python3 - "$env_file" <<'PY'
import shlex
import sys
from pathlib import Path

value = ""
for line in Path(sys.argv[1]).read_text().splitlines():
    name, sep, raw = line.partition("=")
    if sep and name.strip() == "ECHOEAR_BRIDGE_TOKEN":
        value = raw
try:
    parts = shlex.split(value)
except ValueError:
    raise SystemExit("Invalid bridge token assignment") from None
if len(parts) > 1:
    raise SystemExit("Invalid bridge token assignment")
print(parts[0] if parts else "")
PY
    )
fi
if [[ ! ${ECHOEAR_BRIDGE_TOKEN:-} =~ ^[[:graph:]]+$ ]]; then
    echo "Set ECHOEAR_BRIDGE_TOKEN or add it to $env_file." >&2
    exit 1
fi
base="http://$host"
checked_partition() {
    python3 -c '
import json, sys
status = json.load(sys.stdin)
board = status.get("board")
if board != sys.argv[1]:
    actual = board or "unknown board"
    raise SystemExit(f"OTA refused: expected {sys.argv[1]}, got {actual}")
print(status["running_partition"])
if len(sys.argv) > 2 and (status.get("firmware") != sys.argv[2] or
                          status.get("ws_connected") is not True or
                          status.get("ota_pending") is True):
    raise SystemExit(1)
' "$board_name" "$@"
}
before=$(curl --noproxy '*' -fsS --max-time 5 "$base/status")
slot=$(checked_partition <<<"$before")
if (( build )); then
    "$scripts/build.sh"
fi
image="$scripts/../firmware/$build_dir/$board_image"
[[ -s "$image" ]] || { echo "Firmware image not found: $image" >&2; exit 1; }
version=$(python3 - "$image" <<'PY'
import sys

with open(sys.argv[1], "rb") as image:
    header = image.read(80)
# ESP-IDF image header (24), first segment header (8), app description
# magic (4), secure version and reserved (12), version (32).
if len(header) != 80 or header[0] != 0xe9 or header[32:36] != b"\x32\x54\xcd\xab":
    raise SystemExit("Missing ESP-IDF app image descriptor")
version = header[48:80].split(b"\0", 1)[0].decode("ascii")
if not version or not version.isprintable():
    raise SystemExit("Invalid firmware version in app image")
print(version)
PY
)

# Pass the header via stdin so the token is not an argument in the process list.
printf 'X-Lark-Token: %s\n' "$ECHOEAR_BRIDGE_TOKEN" |
    curl --noproxy '*' -fsS --max-time 180 -H @- -H 'Expect:' -H 'Content-Type: application/octet-stream' \
        --data-binary "@$image" "$base/ota/upload"

for (( attempt=0; attempt<60; attempt++ )); do
    if status=$(curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 "$base/status" 2>/dev/null) &&
        current=$(checked_partition "$version" <<<"$status") &&
        [[ "$current" != "$slot" && "$current" =~ ^ota_[01]$ ]]; then
        printf '%s\n' "$status"
        exit 0
    fi
    sleep 1
done
echo "Device did not confirm firmware $version on a different OTA partition with a bridge connection: $base/status" >&2
exit 1
