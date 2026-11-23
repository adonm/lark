#!/usr/bin/env bash
# Shared release build for `lark deploy` and scripts/setup-host.sh.
# Sourcing this file defines lark_build; running it builds to OUTPUT.
#
# The Parakeet bridge build is cached in ~/.cache/lark/parakeet-native, so a
# redeploy reuses the compiled native libraries instead of a fresh CMake build.
set -euo pipefail
umask 077

lark_build() {
    local output=${1-}
    if [[ -z $output ]]; then
        printf 'Usage: %s OUTPUT\n' "$0" >&2
        return 2
    fi
    local repo data_home data cache stt_stage native
    local -a files
    repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
    data_home=${XDG_DATA_HOME:-$HOME/.local/share}
    data=$data_home/lark
    cache=${XDG_CACHE_HOME:-$HOME/.cache}/lark
    # shellcheck source=scripts/go-bin.sh
    source "$repo/scripts/go-bin.sh"
    local GO_BIN
    GO_BIN=$(resolve_go)
    install -d -m 700 -- "$cache"
    if [[ ! -d $(dirname -- "$output") ]]; then
        mkdir -p -- "$(dirname -- "$output")"
    fi
    stt_stage=$cache/parakeet-native
    install -d -m 700 -- "$stt_stage" "$data/lib" "$data/stt-models"
    "$repo/scripts/build-parakeet.sh" "$stt_stage" "$data/stt-models"
    (
        cd -- "$repo/lark"
        export CGO_ENABLED=1
        export CGO_CFLAGS="\"-I$stt_stage/native/include\"${CGO_CFLAGS:+ $CGO_CFLAGS}"
        export CGO_LDFLAGS="\"-L$stt_stage/native/lib\" -Wl,-rpath,\$ORIGIN/lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
        "$GO_BIN" mod download github.com/k2-fsa/sherpa-onnx-go-linux
        "$GO_BIN" mod verify
        "$GO_BIN" build -tags parakeet,sqlite_fts5 -trimpath -ldflags="-s -w -extldflags \"-Wl,-rpath,\$ORIGIN/lib\"" -o "$output" .
        case $("$GO_BIN" env GOARCH) in
            amd64) native=x86_64-unknown-linux-gnu ;;
            arm64) native=aarch64-unknown-linux-gnu ;;
            arm) native=arm-unknown-linux-gnueabihf ;;
            *) printf 'Unsupported Linux architecture for the sherpa-onnx native libraries.\n' >&2; exit 1 ;;
        esac
        local module libs
        module=$("$GO_BIN" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)
        libs=$module/lib/$native
        files=(libsherpa-onnx-c-api.so libonnxruntime.so)
        if [[ -f "$libs/libsherpa-onnx-cxx-api.so" ]]; then
            files+=(libsherpa-onnx-cxx-api.so)
        fi
        (
            cd -- "$libs"
            sha256sum -- "${files[@]}" > "$stt_stage/SHA256SUMS.sherpa"
            for lib in "${files[@]}"; do
                install -m 755 -- "$lib" "$data/lib/.$lib.new"
                mv -fT -- "$data/lib/.$lib.new" "$data/lib/$lib"
            done
        )
        (cd -- "$data/lib"; sha256sum --check --status "$stt_stage/SHA256SUMS.sherpa")
        install -m 644 -- "$stt_stage/SHA256SUMS.sherpa" "$data/lib/SHA256SUMS.sherpa"
        local notices="$data/lib/licenses/parakeet"
        install -D -m 644 -- "$stt_stage/source/LICENSE" "$notices/transcribe.cpp.txt"
        install -D -m 644 -- "$stt_stage/source/ggml/LICENSE" "$notices/ggml.txt"
        install -D -m 644 -- "$stt_stage/native/share/licenses/transcribe/LICENSE.silero_vad" "$notices/silero-vad.txt"
    )
    chmod 700 -- "$output"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    lark_build "$@"
fi
