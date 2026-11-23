#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
firmware=$(cd -- "$scripts/../firmware" && pwd -P)
if (( $# > 1 )); then
    printf 'Usage: %s [vocat|box3]\n' "$0" >&2
    exit 2
fi
BOARD=${1:-${BOARD:-vocat}}
# shellcheck source=scripts/board.sh
source "$scripts/board.sh"
if [[ ! -f "$firmware/sdkconfig.local" ]]; then
    echo "Copy firmware/sdkconfig.local.example to firmware/sdkconfig.local and fill in your credentials." >&2
    exit 1
fi

if command -v podman >/dev/null 2>&1; then
    podman=(podman)
elif command -v docker >/dev/null 2>&1; then
    podman=(docker)
elif command -v host-spawn >/dev/null 2>&1; then
    podman=(host-spawn podman)
else
    echo "Podman or Docker is required to build the firmware." >&2
    exit 1
fi

# Let the container shell expand its own arguments and paths.
# shellcheck disable=SC2016
exec "${podman[@]}" run --rm --user 0 \
    -v "$firmware:/project:z" -w /project \
    docker.io/espressif/idf:release-v5.5 \
    bash -ec '
        umask 077
        build="/project/$1"
        project="/project/$2"
        defaults=(/project/sdkconfig.defaults)
        if [[ $2 != . ]]; then defaults+=("$project/sdkconfig.defaults"); fi
        defaults+=(/project/sdkconfig.local)
        # Kconfig defaults do not override an existing sdkconfig.
        actions=(build)
        for file in "${defaults[@]}"; do
            if [[ $file -nt "$build/sdkconfig" ]]; then
                rm -f "$build/sdkconfig"
                actions=(reconfigure build)
                break
            fi
        done
        IFS=";"
        idf.py -C "$project" -B "$build" -D "SDKCONFIG=$build/sdkconfig" \
            -D "SDKCONFIG_DEFAULTS=${defaults[*]}" "${actions[@]}"
    ' -- "$build_dir" "$project_dir"
