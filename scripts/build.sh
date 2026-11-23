#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
firmware=$(cd -- "$scripts/../firmware" && pwd -P)
force=false
board_arg=""
for arg in "$@"; do
    case $arg in
        vocat|box3) board_arg=$arg ;;
        --force) force=true ;;
        *)
            printf 'Usage: %s [vocat|box3] [--force]\n' "$0" >&2
            exit 2
            ;;
    esac
done
BOARD=${board_arg:-${BOARD:-vocat}}
# shellcheck source=scripts/board.sh
source "$scripts/board.sh"
if [[ ! -f "$firmware/sdkconfig.local" ]]; then
    echo "Copy firmware/sdkconfig.local.example to firmware/sdkconfig.local and fill in your credentials." >&2
    exit 1
fi
idf_image=docker.io/espressif/idf:release-v5.5
# Incremental builds reuse ccache across runs; the managed components and
# build*/ directories already live in the repo tree.
ccache_dir=${XDG_CACHE_HOME:-$HOME/.cache}/lark-ccache

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

# Everything that changes the image: tracked board sources and config, any
# new unignored source, the local credentials, and this recipe. Generated
# directories (managed_components, build*) are ignored by git and stay out.
source_hash() {
    {
        cd -- "$firmware"
        {
            git ls-files -z --cached --others --exclude-standard -- \
                main boards CMakeLists.txt partitions.csv dependencies.lock sdkconfig.defaults
            if [[ -f sdkconfig.local ]]; then printf 'sdkconfig.local\0'; fi
        } | sort -zu | while IFS= read -r -d '' file; do sha256sum -- "$file"; done
        sha256sum -- "$scripts/build.sh"
        printf 'board=%s image=%s\n' "$BOARD" "$idf_image"
    } | sha256sum | cut -d' ' -f1
}

build_once() {
    local started=$SECONDS hash stamp
    hash=$(source_hash)
    stamp="$firmware/$build_dir/.lark-source-hash"
    if ! "$force" && [[ -f $stamp && $(<"$stamp") == "$hash" && -f "$firmware/$build_dir/$board_image" ]]; then
        printf '%s: up to date (%s, source %s)\n' "$BOARD" "$board_image" "${hash:0:12}"
        return 0
    fi
    install -d -m 700 -- "$ccache_dir"
    # Let the container shell expand its own arguments and paths.
    # shellcheck disable=SC2016
    "${podman[@]}" run --rm --user 0 \
        --env IDF_CCACHE_ENABLE=1 --env CCACHE_DIR=/root/.ccache --env CCACHE_BASEDIR=/project \
        -v "$firmware:/project:z" -v "$ccache_dir:/root/.ccache:z" -w /project \
        "$idf_image" \
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
    printf '%s' "$hash" > "$stamp"
    printf '%s: built in %ss (%s)\n' "$BOARD" "$((SECONDS - started))" "$board_image"
}

build_once
