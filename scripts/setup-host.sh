#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/lib/fetch.sh
source "$REPO_DIR/scripts/lib/fetch.sh"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data_home=${XDG_DATA_HOME:-$HOME/.local/share}
data=$data_home/lark
units=$config/systemd/user
env_file=$config/lark/lark.env
pin_gpu=false
models_only=false
usage() {
    printf '%s\n' 'Usage: setup-host.sh [--gpu-index INDEX]' \
        '       setup-host.sh --models-only  # Kokoro and Silero; no service changes' \
        'The library planner keeps 20% of the data volume free (see lark status and' \
        'lark library --plan); run lark library --download --build to provision it.' \
        'Run vulkaninfo --summary to find the intended Vulkan GPU index.'
}
while (($#)); do
    case "$1:${2-}" in
        --gpu-index:*)
            [[ ${2-} =~ ^[0-9]+$ ]] || { usage >&2; exit 2; }
            pin_gpu=$2; shift 2 ;;
        --models-only:*) models_only=true; shift ;;
        -h:*|--help:*) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
done

if "$models_only" && [[ $pin_gpu != false ]]; then
    usage >&2
    exit 2
fi
for cmd in curl sha256sum tar; do
    command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
done
if ! "$models_only"; then
    # shellcheck source=scripts/go-bin.sh
    source "$REPO_DIR/scripts/go-bin.sh"
    GO_BIN=$(resolve_go)
    for cmd in gcc c++ cmake glslc ninja node npm python3 systemctl ffmpeg; do
        command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
    done
    # shellcheck source=scripts/lib/spirv.sh
    source "$REPO_DIR/scripts/lib/spirv.sh"
    if ! use_spirv_headers; then
        echo "Install SPIRV-Headers (or set CMAKE_PREFIX_PATH) before running setup." >&2
        exit 1
    fi
    systemctl --user show-environment >/dev/null
fi

install_kokoro() (
    local name=kokoro-multi-lang-v1_0
    local checksum=60703707bc05ccd48d60e1eb723f38b07d6e474795a0b6a588d7220400e0f7a4
    local models="$data/tts-models" destination stage archive
    destination="$models/$name"
    if [[ -s "$destination/model.onnx" && -s "$destination/voices.bin" && -s "$destination/tokens.txt" &&
        -s "$destination/espeak-ng-data/phondata" ]]; then
        return 0
    fi
    if [[ -e "$destination" || -L "$destination" ]]; then
        printf 'Move the incomplete Kokoro model at %s aside before running setup.\n' "$destination" >&2
        return 1
    fi
    mkdir -p -- "$models"
    stage=$(mktemp -d "$models/.$name.XXXXXXXX")
    trap 'rm -rf -- "$stage"' EXIT
    archive="$stage/$name.tar.bz2"
    if ! download_verified \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/$name.tar.bz2" \
        "$archive" "$checksum"; then
        printf 'Kokoro model archive failed SHA-256 verification.\n' >&2
        return 1
    fi
    tar -xjf "$archive" -C "$stage"
    mv -nT -- "$stage/$name" "$destination"
)
install_kokoro

install_silero() (
    local name=silero_vad_v5.onnx checksum=6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396
    local models="$data/stt-models" destination
    destination="$models/$name"
    if [[ -f "$destination" ]] && verify_sha256 "$checksum" "$destination"; then
        return 0
    fi
    mkdir -p -- "$models"
    if ! download_verified \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/$name" \
        "$destination" "$checksum"; then
        printf 'Silero v5 model failed SHA-256 verification.\n' >&2
        return 1
    fi
)
install_silero

install_smart_turn() (
    local name=smart-turn-v3.2-cpu.onnx checksum=2bb026316b14a660486a75b1733cd3fbab8c2fd0314dc9af7be49f8cca967e4f
    local models="$data/stt-models" destination
    destination="$models/$name"
    if [[ -f "$destination" ]] && verify_sha256 "$checksum" "$destination"; then
        return 0
    fi
    mkdir -p -- "$models"
    if ! download_verified \
        "https://huggingface.co/pipecat-ai/smart-turn-v3/resolve/f766f81d3cfdf7737ac64aad813d91bbfd56bf93/$name" \
        "$destination" "$checksum"; then
        printf 'Smart Turn v3.2 model failed SHA-256 verification.\n' >&2
        return 1
    fi
)
install_smart_turn
if "$models_only"; then
    exit 0
fi

install_deltachat_rpc() (
    local version=v2.56.0
    local bin_dir="$data/bin" dest="$data/bin/deltachat-rpc-server" arch checksum url staged
    case $(uname -m) in
        x86_64)
            arch=x86_64-linux
            checksum=312d4753160ca0c50f32d3e4f6de4fcaf91527f53980e04f52505a6bf951843c
            ;;
        *)
            printf 'Unsupported architecture %s for deltachat-rpc-server.\n' "$(uname -m)" >&2
            return 1
            ;;
    esac
    if [[ -x "$dest" ]] && verify_sha256 "$checksum" "$dest"; then
        return 0
    fi
    install -d -m 700 -- "$bin_dir"
    staged=$(mktemp "$bin_dir/.dc-rpc.XXXXXXXX")
    trap 'rm -f -- "$staged"' EXIT
    url="https://github.com/chatmail/core/releases/download/$version/deltachat-rpc-server-$arch"
    curl --fail --silent --show-error --location --output "$staged" "$url"
    if ! verify_sha256 "$checksum" "$staged"; then
        printf 'deltachat-rpc-server failed SHA-256 verification.\n' >&2
        return 1
    fi
    chmod 700 -- "$staged"
    mv -fT -- "$staged" "$dest"
)
install_deltachat_rpc

lark_bin="$data/lark-edge"
library_bin="$data/lark-library"
build_tmp=$(mktemp "$lark_bin.XXXXXXXX")
library_tmp=$(mktemp "$library_bin.XXXXXXXX")
trap 'rm -f -- "$build_tmp" "$library_tmp"' EXIT
# shellcheck source=scripts/build-lark.sh
source "$REPO_DIR/scripts/build-lark.sh"
lark_build "$build_tmp" "$library_tmp"
mv -f -- "$build_tmp" "$lark_bin"
mv -f -- "$library_tmp" "$library_bin"
trap - EXIT

(cd "$REPO_DIR/brain" && npm ci --no-audit --no-fund)
# Song recognition is a pinned MIT WASM package, not a system binary.
(cd "$REPO_DIR/brain" && node -e 'require.resolve("shazamio-core")') ||
    { echo 'shazamio-core is missing; song recognition will be unavailable.' >&2; exit 1; }

mkdir -p -- "$units" "$config/lark"
wrap="$data/lark"
# Replace the v1 server binary (or a stale symlink) with the CLI wrapper.
ln -sfn -- "$REPO_DIR/scripts/lark" "$wrap"
python3 "$REPO_DIR/scripts/setup/lark_env.py" "$env_file" "$pin_gpu"
node_bin=$(type -P node)
python3 "$REPO_DIR/scripts/setup/systemd_units.py" "$REPO_DIR" "$data_home" \
    "$lark_bin" "$node_bin" "$units" "$env_file"
python3 "$REPO_DIR/scripts/setup/import_credentials.py" \
    "$data_home/opencode/auth.json" "$data/opencode/auth.json"
systemctl --user daemon-reload
retire_unit() {
    local unit=$1
    if [[ -e $units/$unit ]]; then
        # Separate disable/stop: `--now` refuses to run where sd_booted()
        # reports false (containers) even though the user manager is up.
        systemctl --user disable "$unit"
        systemctl --user stop "$unit"
        rm -f -- "$units/$unit"
    fi
}
retire_unit lark.service
retire_unit lark-opencode.service
systemctl --user daemon-reload
systemctl --user enable lark-edge.service lark-brain.service
systemctl --user restart lark-edge.service lark-brain.service

printf '%s\n' 'Default voices: Heart (af_heart); Mint uses Fable (bm_fable).'
printf '%s\n' 'lark-edge owns terminals and speech models; lark-brain owns companions, Delta Chat, and OpenCode.'
printf 'Configure firmware/sdkconfig.local as shown in README.md. The Lark token is in %s.\n' "$env_file"
