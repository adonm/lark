# shellcheck shell=bash
# Gate phases shared by scripts/check.sh and scripts/check-fast.sh. The caller
# resolves GO_BIN first and exports CGO_ENABLED=1.
host_tests=$(mktemp -d "${TMPDIR:-/tmp}/lark-check.XXXXXXXX")
trap 'rm -rf -- "$host_tests"' EXIT
host_cc=(gcc -std=c11 -Wall -Wextra -Werror -I firmware/main "-fsanitize=address,undefined"
         -fno-sanitize-recover=all -fno-pie -no-pie)

phase_shell() {
    local script
    for script in scripts/*.sh scripts/lib/*.sh; do
        bash -n "$script"
    done
    if command -v shellcheck >/dev/null; then
        shellcheck -x scripts/*.sh scripts/lib/*.sh
    fi
    printf 'PASS shell checks\n'
}

phase_node() {
    node --test config/opencode/*.test.mjs
    node --check lark/web/remote/app.js
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

phase_live_compile() {
    # Compile the live suite so build-tag drift cannot land unnoticed.
    (cd lark && "$GO_BIN" test -tags=live,sqlite_fts5 -run '^$' -count=1 .)
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

phase_build() { (cd lark && "$GO_BIN" build -tags sqlite_fts5 ./...); }
