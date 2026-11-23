#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/uvx.sh
source "$scripts/uvx.sh"
# shellcheck source=scripts/board.sh
source "$scripts/board.sh"
if (( $# > 1 )); then
    echo "Usage: $0 [serial-device]  # default /dev/ttyACM0 when unambiguous" >&2
    exit 2
fi
port=${1-}
if [[ -z "$port" ]]; then
    shopt -s nullglob
    ports=(/dev/ttyACM*)
    if [[ ${BOARD:-vocat} != vocat || ${#ports[@]} != 1 ]]; then
        echo "Specify the USB port for $board_name: $0 /dev/ttyACM0" >&2
        exit 2
    fi
    port=${ports[0]}
fi
build="$scripts/../firmware/$build_dir"
if [[ ! -s "$build/$board_image" || ! -s "$build/flash_args" ]]; then
    echo "Build $board_name with BOARD=${BOARD:-vocat} scripts/build.sh before flashing." >&2
    exit 1
fi
cd -- "$build"
echo "Flashing $board_name on $port"
run_uvx --from esptool esptool --chip esp32s3 --port "$port" --baud 460800 \
    write-flash --no-progress @flash_args
