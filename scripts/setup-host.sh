#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data_home=${XDG_DATA_HOME:-$HOME/.local/share}
data=$data_home/lark
units=$config/systemd/user
oc_root=$data/opencode
env_file=$config/lark/bridge.env
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
    for cmd in gcc c++ cmake glslc ninja opencode python3 systemctl; do
        command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
    done
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

bridge_bin="$data/lark-bridge"
build_tmp=$(mktemp "$bridge_bin.XXXXXXXX")
stt_stage=$(mktemp -d)
trap 'rm -f -- "$build_tmp"; rm -rf -- "$stt_stage"' EXIT
"$REPO_DIR/scripts/build-parakeet.sh" "$stt_stage" "$data/stt-models"
(
    cd -- "$REPO_DIR/bridge"
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
mv -f -- "$build_tmp" "$bridge_bin"
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
python3 "$REPO_DIR/scripts/setup-opencode-config.py" "$oc_root/config/opencode" "$bridge_bin"

python3 - "$env_file" "$pin_gpu" <<'PY'
import os
import re
import secrets
import sys
import tempfile
from pathlib import Path

path = Path(sys.argv[1])
old = path.read_text() if path.exists() else (
    "LARK_BRIDGE_HOST=0.0.0.0\nLARK_BRIDGE_PORT=8765\n"
    f"LARK_BRIDGE_TOKEN={secrets.token_hex(32)}\n"
)
prior = re.findall(r"^LARK_OPENCODE_PASSWORD=([a-f0-9]{64})$", old, re.M)
password = prior[-1] if prior else secrets.token_hex(32)
settings = {
    "LARK_OPENCODE_URL": "http://127.0.0.1:49375",
    "LARK_OPENCODE_PASSWORD": password,
    "OPENCODE_SERVER_PASSWORD": password,
    "LARK_OPENCODE_PAIR_PORT": "49376",
}
if sys.argv[2] != "false":
    settings["GGML_VK_VISIBLE_DEVICES"] = sys.argv[2]
kept = [line for line in old.splitlines(keepends=True)
        if line.strip().partition("=")[0].rstrip() not in settings.keys() | {"LARK_STT", "LARK_SILERO_VAD_MODEL"}]
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

python3 - "$REPO_DIR" "$data_home" "$bridge_bin" "$units" \
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
paths = {"REPO_DIR": repo, "DATA_HOME": data, "BRIDGE_BIN": binary,
         "OPENCODE_BIN": opencode, "ENV_FILE": env_file}
for name in ("lark-bridge.service", "lark-opencode.service",
             "lark-opencode-lan.socket", "lark-opencode-lan.service"):
    template = Path(repo, "bridge", name).read_text()
    rendered = re.sub(r"@([A-Z_]+)@", lambda m: escape(paths[m[1]]), template)
    if name == "lark-opencode.service":
        if Path(oc_root) != Path.home() / ".local/share/lark/opencode":
            rendered = rendered.replace("%h/.local/share/lark/opencode", escape(oc_root))
            rendered = rendered.replace(f"WorkingDirectory={escape(oc_root)}/workspace",
                                        f'WorkingDirectory="{escape(oc_root)}/workspace"')
        if Path(env_file) != Path.home() / ".config/lark/bridge.env":
            rendered = rendered.replace("EnvironmentFile=%h/.config/lark/bridge.env",
                                        f"EnvironmentFile={escape(env_file)}")
    path = units / name
    path.write_text(rendered)
    path.chmod(0o644)

PY

systemctl --user daemon-reload
systemctl --user enable lark-opencode.service lark-opencode-lan.socket lark-bridge.service
systemctl --user restart lark-opencode.service lark-bridge.service
systemctl --user start lark-opencode-lan.socket

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
    with client.open(request, timeout=1) as response:
        return json.load(response)

deadline = time.monotonic() + 10
while True:
    try:
        get("/api/info")
        break
    except OSError:
        if time.monotonic() >= deadline:
            sys.exit("Dedicated OpenCode API did not become ready with Basic auth")
        time.sleep(0.2)

try:
    primary = get("/api/agent/lark")["data"]["model"]["providerID"]
    router = next(p for entry in get("/api/config")
                  if entry.get("path") == str(Path(root, "config/opencode/opencode.jsonc"))
                  for p in entry["info"]["plugins"]
                  if isinstance(p, dict) and p.get("package") == "./lark-model-router")
    fallback = router["options"]["fallback"].split("/", 1)[0]
except (OSError, KeyError, StopIteration, TypeError, ValueError):
    sys.exit("Could not read the Lark model settings from the dedicated OpenCode config")
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
printf 'Configure firmware/sdkconfig.local as shown in README.md. The bridge token is in %s.\n' "$env_file"
