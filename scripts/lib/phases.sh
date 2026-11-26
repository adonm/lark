# shellcheck shell=bash
# Gate phases shared by scripts/check.sh and scripts/check-fast.sh. The caller
# resolves GO_BIN first and exports CGO_ENABLED=1.
host_tests=$(mktemp -d "${TMPDIR:-/tmp}/lark-check.XXXXXXXX")
trap 'rm -rf -- "$host_tests"' EXIT
host_cc=(gcc -std=c11 -Wall -Wextra -Werror -I firmware/main "-fsanitize=address,undefined"
         -fno-sanitize-recover=all -fno-pie -no-pie)

phase_shell() {
    local script
    for script in scripts/*.sh scripts/lib/*.sh scripts/lark; do
        bash -n "$script"
    done
    if command -v shellcheck >/dev/null; then
        shellcheck -x scripts/*.sh scripts/lib/*.sh scripts/lark
    fi
    printf 'PASS shell checks\n'
}

phase_brain() {
    # Typecheck first, then the whole suite. e2e.test.ts builds and runs the
    # real Go edge in a separate process, so this covers the IPC seam too.
    (cd brain && npm run --silent typecheck && node --test src/*.test.ts)
    node --check web/remote/app.js
    printf 'PASS brain typecheck and tests\n'
}

phase_python() { python3 -m unittest discover -s scripts/tests; }

phase_vet() {
    (
        cd lark || exit
        "$GO_BIN" mod verify
        # No Vulkan toolchain, weights or GPU are needed for the offline suite.
        "$GO_BIN" vet ./...
    )
}

phase_race() { (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 ./...); }

phase_parakeet_compile() {
    # The cgo bridge only builds where Parakeet was compiled, so tag-gated
    # breakage otherwise waits for a deploy. Compile it when the cached native
    # bridge (or LARK_PARAKEET_NATIVE) is present and skip cleanly elsewhere,
    # keeping CI and hosts without Parakeet green.
    local cache native
    cache=${XDG_CACHE_HOME:-$HOME/.cache}/lark/parakeet-native/native
    native=${LARK_PARAKEET_NATIVE:-}
    if [[ -z $native && -d $cache ]]; then
        native=$cache
    fi
    if [[ -z $native || ! -d $native/include || ! -d $native/lib ]]; then
        printf 'SKIP parakeet compile (native bridge not built)\n'
        return 0
    fi
    native=$(realpath -- "$native")
    (
        cd lark || exit
        CGO_CFLAGS="\"-I$native/include\"${CGO_CFLAGS:+ $CGO_CFLAGS}" \
        CGO_LDFLAGS="\"-L$native/lib\"${CGO_LDFLAGS:+ $CGO_LDFLAGS}" \
            "$GO_BIN" build -tags 'parakeet' -o /dev/null ./cmd/lark-edge
    )
    printf 'PASS parakeet compile (%s)\n' "$native"
}

phase_c_host() {
    "${host_cc[@]}" firmware/tests/vad_test.c firmware/main/vad.c -o "$host_tests/vad"
    "$host_tests/vad"
    printf 'PASS host VAD tests\n'

    "${host_cc[@]}" firmware/tests/touch_gesture_test.c firmware/main/touch_gesture.c -o "$host_tests/touch_gesture"
    "$host_tests/touch_gesture"

    "${host_cc[@]}" firmware/tests/proto_test.c -o "$host_tests/proto"
    "$host_tests/proto"

    "${host_cc[@]}" firmware/tests/wake_gate_test.c -o "$host_tests/wake_gate"
    "$host_tests/wake_gate"

    "${host_cc[@]}" firmware/tests/net_logic_test.c firmware/main/net_logic.c -o "$host_tests/net_logic"
    "$host_tests/net_logic"

    "${host_cc[@]}" firmware/tests/telemetry_logic_test.c firmware/main/telemetry_logic.c -o "$host_tests/telemetry_logic" && "$host_tests/telemetry_logic"

    "${host_cc[@]}" firmware/tests/audio_logic_test.c firmware/main/audio_logic.c -o "$host_tests/audio_logic"
    "$host_tests/audio_logic"

    "${host_cc[@]}" firmware/tests/debug_parse_test.c -o "$host_tests/debug_parse"
    "$host_tests/debug_parse"

    local board flags
    for board in vocat box3; do
        flags=()
        if [[ "$board" == box3 ]]; then flags+=(-DLARK_BOX3=1); fi
        printf 'Host frame and graphics tests (%s)\n' "$board"
        "${host_cc[@]}" "${flags[@]}" firmware/tests/frame_test.c firmware/main/frame.c -o "$host_tests/frame"
        "$host_tests/frame"
        "${host_cc[@]}" "${flags[@]}" firmware/tests/gfx_test.c firmware/main/gfx.c -o "$host_tests/gfx"
        "$host_tests/gfx"
        "${host_cc[@]}" "${flags[@]}" firmware/tests/display_logic_test.c firmware/main/display_logic.c -o "$host_tests/display_logic"
        "$host_tests/display_logic"
    done
}

phase_build() { (cd lark && "$GO_BIN" build ./...); }
