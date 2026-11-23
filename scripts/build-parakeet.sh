#!/usr/bin/env bash
set -euo pipefail
umask 077
if [[ $# != 2 || ! -d $1 ]]; then
    printf 'Usage: build-parakeet.sh EXISTING_BUILD_DIR MODEL_DIR\n' >&2
    exit 2
fi
build=$1
models=$2
model=parakeet-unified-en-0.6b-Q8_0.gguf
model_sha=4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795
model_rev=7e948f21b7bdbac698d3318db9d350f1096f3b6c
source_sha=54fe2a5b2fb43662e6d5034585dd44012a67876ae382a49d124ab02d92162a88
source_rev=bcb639faa4c74a2300ba19e30360cb2355b1663a
# Bump when the CMake flags or patches below change; a stale cache is rebuilt.
cache_rev=1

verify() { printf '%s  %s\n' "$1" "$2" | sha256sum --check --status; }
download() { curl --fail --silent --show-error --location --output "$1" "$2"; }

if [[ -f $build/cache-rev && $(<"$build/cache-rev") == "$cache_rev-$source_rev" &&
    -f $build/native/lib/libtranscribe.a ]] &&
    (cd -- "$build/native" && sha256sum --check --status SHA256SUMS); then
    printf 'Parakeet native bridge up to date (%s)\n' "$build/native"
    exit 0
fi

command -v glslc >/dev/null || {
    printf 'glslc is required on PATH for Parakeet Vulkan shader compilation.\n' >&2
    exit 1
}

# shellcheck source=scripts/lib/spirv.sh
source "$(dirname -- "${BASH_SOURCE[0]}")/lib/spirv.sh"
if ! use_spirv_headers; then
    printf 'SPIRV-HeadersConfig.cmake not found (install spirv-headers or set CMAKE_PREFIX_PATH).\n' >&2
    exit 1
fi

rm -rf -- "$build/source" "$build/build" "$build/native" "$build/cache-rev"
if ! [[ -f $build/source.tar.gz ]] || ! verify "$source_sha" "$build/source.tar.gz"; then
    rm -f -- "$build/source.tar.gz"
    download "$build/source.tar.gz" "https://codeload.github.com/anomalyco/transcribe.cpp/tar.gz/$source_rev"
    if ! verify "$source_sha" "$build/source.tar.gz"; then
        printf 'transcribe.cpp source archive failed SHA-256 verification.\n' >&2
        exit 1
    fi
fi
mkdir -- "$build/source"
tar -xzf "$build/source.tar.gz" -C "$build/source" --strip-components=1
cmake -S "$build/source" -B "$build/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
    -DTRANSCRIBE_BUILD_TESTS=OFF -DTRANSCRIBE_BUILD_EXAMPLES=OFF -DTRANSCRIBE_BUILD_SHARED=OFF \
    -DTRANSCRIBE_USE_SYSTEM_BLAS=OFF -DTRANSCRIBE_VULKAN=ON -DGGML_NATIVE=OFF -DGGML_CCACHE=OFF \
    -DGGML_VULKAN_SHADER_TYPES=f32,f16,q8_0,q5_0 -DGGML_VULKAN_MUL_MAT_ID=OFF
cmake --build "$build/build" --parallel 4
cmake --install "$build/build" --prefix "$build/native"
(cd -- "$build/native"; sha256sum lib/*.a > SHA256SUMS)
printf '%s' "$cache_rev-$source_rev" > "$build/cache-rev"

mkdir -p -- "$models"
if [[ -f $models/$model ]] && verify "$model_sha" "$models/$model"; then
    exit 0
fi
stage=$(mktemp -d "$models/.$model.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
download "$stage/$model" "https://huggingface.co/handy-computer/parakeet-unified-en-0.6b-gguf/resolve/$model_rev/$model?download=true"
if ! verify "$model_sha" "$stage/$model"; then
    printf 'Parakeet model failed SHA-256 verification.\n' >&2
    exit 1
fi
mv -fT -- "$stage/$model" "$models/$model"
