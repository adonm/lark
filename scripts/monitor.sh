#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/uvx.sh
source "$scripts/uvx.sh"
if (( $# > 1 )); then
    echo "Usage: $0 [serial-device]  # default /dev/ttyACM0" >&2
    exit 2
fi
run_uvx --from pyserial pyserial-miniterm "${1:-/dev/ttyACM0}" 115200
