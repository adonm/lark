#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data_home=${XDG_DATA_HOME:-$HOME/.local/share}
data=$data_home/lark
units=$config/systemd/user
oc_root=$data/opencode
env_file=$config/lark/lark.env
pin_gpu=false
models_only=false
usage() {
    printf '%s\n' 'Usage: setup-host.sh [--gpu-index INDEX]' \
        '       setup-host.sh --models-only  # Kokoro and Silero; no service changes' \
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
    for cmd in gcc c++ cmake glslc ninja opencode python3 systemctl ffmpeg; do
        command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
    done
    spirv_found=false
    IFS=: read -ra prefix_dirs <<<"${CMAKE_PREFIX_PATH:-}:/usr:/usr/local:/home/linuxbrew/.linuxbrew/opt/spirv-headers"
    for dir in "${prefix_dirs[@]}" /run/host/*/*/linuxbrew/.linuxbrew/opt/spirv-headers; do
        [[ -n "$dir" ]] || continue
        if [[ -f "$dir/share/cmake/SPIRV-Headers/SPIRV-HeadersConfig.cmake" ||
              -f "$dir/lib/cmake/SPIRV-Headers/SPIRV-HeadersConfig.cmake" ||
              -f "$dir/lib64/cmake/SPIRV-Headers/SPIRV-HeadersConfig.cmake" ]]; then
            export CMAKE_PREFIX_PATH="$dir${CMAKE_PREFIX_PATH:+:$CMAKE_PREFIX_PATH}"
            spirv_found=true
            break
        fi
    done
    if ! "$spirv_found"; then
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
    curl --fail --silent --show-error --location \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/$name.tar.bz2" \
        --output "$archive"
    if ! printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check --status; then
        printf 'Kokoro model archive failed SHA-256 verification.\n' >&2
        return 1
    fi
    tar -xjf "$archive" -C "$stage"
    mv -nT -- "$stage/$name" "$destination"
)
install_kokoro

install_silero() (
    local name=silero_vad_v5.onnx checksum=6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396
    local models="$data/stt-models" destination staged
    destination="$models/$name"
    verify() { printf '%s  %s\n' "$checksum" "$1" | sha256sum --check --status; }
    if [[ -f "$destination" ]] && verify "$destination"; then
        return 0
    fi
    mkdir -p -- "$models"
    staged=$(mktemp "$models/.$name.XXXXXXXX")
    trap 'rm -f -- "$staged"' EXIT
    curl --fail --silent --show-error --location --output "$staged" \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/$name"
    if ! verify "$staged"; then
        printf 'Silero v5 model failed SHA-256 verification.\n' >&2
        return 1
    fi
    mv -fT -- "$staged" "$destination"
)
install_silero
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
    verify() { printf '%s  %s\n' "$checksum" "$1" | sha256sum --check --status; }
    if [[ -x "$dest" ]] && verify "$dest"; then
        return 0
    fi
    install -d -m 700 -- "$bin_dir"
    staged=$(mktemp "$bin_dir/.dc-rpc.XXXXXXXX")
    trap 'rm -f -- "$staged"' EXIT
    url="https://github.com/chatmail/core/releases/download/$version/deltachat-rpc-server-$arch"
    curl --fail --silent --show-error --location --output "$staged" "$url"
    if ! verify "$staged"; then
        printf 'deltachat-rpc-server failed SHA-256 verification.\n' >&2
        return 1
    fi
    chmod 700 -- "$staged"
    mv -fT -- "$staged" "$dest"
)
install_deltachat_rpc

lark_bin="$data/lark"
build_tmp=$(mktemp "$lark_bin.XXXXXXXX")
stt_stage=$(mktemp -d)
trap 'rm -f -- "$build_tmp"; rm -rf -- "$stt_stage"' EXIT
"$REPO_DIR/scripts/build-parakeet.sh" "$stt_stage" "$data/stt-models"
(
    cd -- "$REPO_DIR/lark"
    export CGO_ENABLED=1
    export CGO_CFLAGS="\"-I$stt_stage/native/include\"${CGO_CFLAGS:+ $CGO_CFLAGS}"
    export CGO_LDFLAGS="\"-L$stt_stage/native/lib\" -Wl,-rpath,\$ORIGIN/lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
    "$GO_BIN" mod download github.com/k2-fsa/sherpa-onnx-go-linux
    "$GO_BIN" mod verify
    "$GO_BIN" build -tags parakeet -trimpath -ldflags="-s -w -extldflags \"-Wl,-rpath,\$ORIGIN/lib\"" -o "$build_tmp" .
    case $("$GO_BIN" env GOARCH) in
        amd64) native=x86_64-unknown-linux-gnu ;;
        arm64) native=aarch64-unknown-linux-gnu ;;
        arm) native=arm-unknown-linux-gnueabihf ;;
        *) printf 'Unsupported Linux architecture for Kokoro.\n' >&2; exit 1 ;;
    esac
    module=$("$GO_BIN" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)
    libs=$module/lib/$native
    files=(libsherpa-onnx-c-api.so libonnxruntime.so)
    if [[ -f "$libs/libsherpa-onnx-cxx-api.so" ]]; then
        files+=(libsherpa-onnx-cxx-api.so)
    fi
    install -d -m 700 -- "$data/lib"
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
    notices="$data/lib/licenses/parakeet"
    install -D -m 644 -- "$stt_stage/source/LICENSE" "$notices/transcribe.cpp.txt"
    install -D -m 644 -- "$stt_stage/source/ggml/LICENSE" "$notices/ggml.txt"
    install -D -m 644 -- "$stt_stage/native/share/licenses/transcribe/LICENSE.silero_vad" "$notices/silero-vad.txt"
)
chmod 700 -- "$build_tmp"
mv -f -- "$build_tmp" "$lark_bin"
rm -rf -- "$stt_stage"
trap - EXIT

mkdir -p -- "$oc_root"/{config/opencode/agents,config/opencode/plugins,data/opencode,state,cache,workspace} \
    "$config/lark" "$units"
link_file() {
    local source=$1 destination=$2
    if [[ "$destination" -ef "$source" ]]; then
        return
    fi
    if [[ -e "$destination" || -L "$destination" ]]; then
        printf 'Move the existing %s aside before running setup.\n' "$destination" >&2
        exit 1
    fi
    ln -s -- "$source" "$destination"
}
link_file "$REPO_DIR/agents/lark.md" "$oc_root/config/opencode/agents/lark.md"
link_file "$REPO_DIR/agents/lark-builder.md" "$oc_root/config/opencode/agents/lark-builder.md"
python3 "$REPO_DIR/scripts/setup-opencode-config.py" "$oc_root/config/opencode" "$lark_bin"

python3 - "$env_file" "$pin_gpu" <<'PY'
import os
import re
import secrets
import sys
import tempfile
from pathlib import Path

path = Path(sys.argv[1])
old = path.read_text() if path.exists() else (
    "LARK_HOST=0.0.0.0\nLARK_PORT=8765\nLARK_DEVICE_PORT=8766\n"
    f"LARK_TOKEN={secrets.token_hex(32)}\n"
)
prior = re.findall(r"^LARK_OPENCODE_PASSWORD=([a-f0-9]{64})$", old, re.M)
password = prior[-1] if prior else secrets.token_hex(32)
settings = {
    "LARK_OPENCODE_URL": "http://127.0.0.1:49375",
    "LARK_OPENCODE_PASSWORD": password,
    "OPENCODE_SERVER_PASSWORD": password,
}
if not re.search(r"^LARK_CHATMAIL_RELAY=", old, re.M):
    settings["LARK_CHATMAIL_RELAY"] = "nine.testrun.org"
if sys.argv[2] != "false":
    settings["GGML_VK_VISIBLE_DEVICES"] = sys.argv[2]
kept = [line for line in old.splitlines(keepends=True)
        if line.strip().partition("=")[0].rstrip() not in settings.keys() | {"LARK_STT", "LARK_SILERO_VAD_MODEL", "LARK_OPENCODE_PAIR_PORT"}]
text = "".join(kept)
if text and not text.endswith("\n"):
    text += "\n"
text += "".join(f"{key}={value}\n" for key, value in settings.items())
with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as f:
    try:
        f.write(text)
        f.close()
        os.replace(f.name, path)
    finally:
        Path(f.name).unlink(missing_ok=True)
PY

python3 - "$REPO_DIR" "$data_home" "$lark_bin" "$units" \
    "$(type -P opencode)" "$env_file" "$oc_root" <<'PY'
import re
import sys
from pathlib import Path

def escape(value):
    return value.translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "\n": "\\n", "\r": "\\r", "\t": "\\t",
    }))

repo, data, binary, directory, opencode, env_file, oc_root = sys.argv[1:]
units = Path(directory)
paths = {"REPO_DIR": repo, "DATA_HOME": data, "LARK_BIN": binary,
         "OPENCODE_BIN": opencode, "ENV_FILE": env_file}
for name in ("lark.service", "lark-opencode.service"):
    template = Path(repo, "lark", name).read_text()
    rendered = re.sub(r"@([A-Z_]+)@", lambda m: escape(paths[m[1]]), template)
    if name == "lark-opencode.service":
        if Path(oc_root) != Path.home() / ".local/share/lark/opencode":
            rendered = rendered.replace("%h/.local/share/lark/opencode", escape(oc_root))
            rendered = rendered.replace(f"WorkingDirectory={escape(oc_root)}/workspace",
                                        f'WorkingDirectory="{escape(oc_root)}/workspace"')
        if Path(env_file) != Path.home() / ".config/lark/lark.env":
            rendered = rendered.replace("EnvironmentFile=%h/.config/lark/lark.env",
                                        f"EnvironmentFile={escape(env_file)}")
    path = units / name
    path.write_text(rendered)
    path.chmod(0o644)

PY

systemctl --user daemon-reload
systemctl --user enable lark-opencode.service lark.service
systemctl --user restart lark-opencode.service lark.service

python3 - "$env_file" "$oc_root" "$(type -P opencode)" <<'PY'
import base64
import json
import os
import re
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

env_file, root, opencode = sys.argv[1:]
settings = dict(re.findall(r"^\s*(LARK_OPENCODE_\w+)\s*=([^\n]*)", Path(env_file).read_text(), re.M))
url = settings["LARK_OPENCODE_URL"]
password = settings["LARK_OPENCODE_PASSWORD"]
auth = base64.b64encode(f"opencode:{password}".encode()).decode()
client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def get(path):
    request = urllib.request.Request(f"{url}{path}", headers={"Authorization": f"Basic {auth}"})
    with client.open(request, timeout=5) as response:
        return json.load(response)

deadline = time.monotonic() + 10
while True:
    try:
        agent = get("/api/agent/lark")
        break
    except OSError:
        if time.monotonic() >= deadline:
            sys.exit("Dedicated OpenCode API did not become ready with Basic auth")
        time.sleep(0.2)

try:
    primary = agent["data"]["model"]["providerID"]
    target_config = Path(root, "config/opencode/opencode.jsonc").resolve()
    router = next(p for entry in get("/api/config")
                  if entry.get("path") and Path(entry["path"]).resolve() == target_config
                  for p in entry["info"].get("plugins") or ()
                  if isinstance(p, dict) and p.get("package") == "./lark-model-router")
    fallback = router["options"]["fallback"].split("/", 1)[0]
except (OSError, KeyError, StopIteration, TypeError, ValueError) as err:
    sys.exit(f"Could not read the Lark model settings from the dedicated OpenCode config: {err}")
for provider in sorted({primary, fallback}):
    if not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*", provider):
        sys.exit("Invalid Lark model provider")
    marker = Path(root, "state", f"imported-provider-{provider}")
    if not marker.exists():
        try:
            credentials = subprocess.run([opencode, "auth", "export", provider], check=True,
                                         capture_output=True, timeout=20).stdout
            if json.loads(credentials):
                subprocess.run([opencode, "--server", url, "auth", "import"], input=credentials,
                               env={**os.environ, "OPENCODE_SERVER_PASSWORD": password}, check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
                marker.touch()
        except (OSError, ValueError, subprocess.SubprocessError):
            sys.exit("Could not import the configured providers' OpenCode credentials")
PY

printf '%s\n' 'Default voices: Heart (af_heart); Mint uses Fable (bm_fable).'
printf '%s\n' 'Lark MCP includes control for volume and sleep on connected devices.'
printf 'Configure firmware/sdkconfig.local as shown in README.md. The Lark token is in %s.\n' "$env_file"
