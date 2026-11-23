#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

case "$#:${1-}" in
    0:|1:--live|1:--e2e-real|1:--deployed|1:--real-model|1:--qa|1:--qa-hw|1:--remote-ui) ;;
    *) printf 'Usage: %s [--live | --e2e-real | --deployed | --real-model | --qa | --qa-hw | --remote-ui]\n' "$0" >&2; exit 2 ;;
esac

for script in scripts/*.sh; do
    bash -n "$script"
done
if command -v shellcheck >/dev/null; then
    shellcheck -x scripts/*.sh
fi
printf 'PASS shell checks\n'

# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
export CGO_ENABLED=1
node --test config/opencode/*.test.mjs
(
    cd lark
    "$GO_BIN" mod verify
    # No Vulkan toolchain, weights or GPU are needed for the offline suite.
    "$GO_BIN" vet ./...
    "$GO_BIN" test -race -tags sqlite_fts5 ./...
)

host_tests=$(mktemp -d "${TMPDIR:-/tmp}/lark-check.XXXXXXXX")
trap 'rm -rf -- "$host_tests"' EXIT
host_cc=(gcc -std=c11 -Wall -Wextra -Werror -I firmware/main "-fsanitize=address,undefined"
         -fno-sanitize-recover=all -fno-pie -no-pie)
"${host_cc[@]}" firmware/tests/vad_test.c firmware/main/vad.c -o "$host_tests/vad"
"$host_tests/vad"
printf 'PASS host VAD tests\n'

"${host_cc[@]}" firmware/tests/touch_gesture_test.c firmware/main/touch_gesture.c -o "$host_tests/touch_gesture"
"$host_tests/touch_gesture"

"${host_cc[@]}" firmware/tests/proto_test.c -o "$host_tests/proto"
"$host_tests/proto"

"${host_cc[@]}" firmware/tests/debug_parse_test.c -o "$host_tests/debug_parse"
"$host_tests/debug_parse"

for board in vocat box3; do
    flags=()
    if [[ "$board" == box3 ]]; then flags+=(-DLARK_BOX3=1); fi
    printf 'Host frame and graphics tests (%s)\n' "$board"
    "${host_cc[@]}" "${flags[@]}" firmware/tests/frame_test.c firmware/main/frame.c -o "$host_tests/frame"
    "$host_tests/frame"
    "${host_cc[@]}" "${flags[@]}" firmware/tests/gfx_test.c firmware/main/gfx.c -o "$host_tests/gfx"
    "$host_tests/gfx"
done

if [[ -n $(git ls-files -- ':(glob)**/sdkconfig.local' ':(glob)**/lark.env') ]]; then
    printf 'sdkconfig.local and lark.env must not be tracked\n' >&2
    exit 1
fi
for view in --cached --; do
    status=0
    # Split the literal so this script passes its own check.
    git grep -lF -e '/var/'home/ "$view" || status=$?
    if (( status != 1 )); then
        printf 'Tracked-file path check failed\n' >&2
        exit 1
    fi
done
printf 'PASS tracked-file hygiene\n'

if [[ ${1-} == --live || ${1-} == --e2e-real || ${1-} == --deployed ]]; then
    native=${LARK_PARAKEET_NATIVE:-}
    if [[ -z $native ]]; then
        data_home=${XDG_DATA_HOME:-$HOME/.local/share}
        model=parakeet-unified-en-0.6b-Q8_0.gguf
        mkdir "$host_tests/models"
        ln -s -- "$data_home/lark/stt-models/$model" "$host_tests/models/$model"
        ./scripts/build-parakeet.sh "$host_tests" "$host_tests/models"
        native=$host_tests/native
    else
        native=$(realpath -- "$native")
    fi
    (cd -- "$native"; sha256sum --check --status SHA256SUMS)
    (
        cd lark
        test_bin="$host_tests/lark"
        CGO_CFLAGS="\"-I$native/include\"${CGO_CFLAGS:+ $CGO_CFLAGS}" \
        CGO_LDFLAGS="\"-L$native/lib\"${CGO_LDFLAGS:+ $CGO_LDFLAGS}" \
            "$GO_BIN" build -tags parakeet,sqlite_fts5 -o "$test_bin" .
        case "$1" in
            --live)
                LARK_TEST_BIN="$test_bin" LARK_E2E_MODE=stub "$GO_BIN" test -tags=live -v -count=1 -run '^TestVirtualE2E$' . ;;
            --e2e-real)
                LARK_TEST_BIN="$test_bin" LARK_E2E_MODE=real "$GO_BIN" test -tags=live -v -count=1 -run '^TestVirtualE2E$' . ;;
            --deployed)
                # Read-only installed-service and OTA checks. Never uploads or flashes.
                LARK_TEST_BIN="$test_bin" "$GO_BIN" test -tags=live -v -count=1 -run '^TestLive' . ;;
        esac
    )
elif [[ ${1-} == --real-model ]]; then
    (cd lark && LARK_REAL_MODEL=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestRealModelLarkCodeMode$' .)
elif [[ ${1-} == --qa ]]; then
    (cd lark && "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQASimSuite$' .)
    if [[ ${LARK_QA_LIBRARY-} == 1 ]]; then
        (cd lark && "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQALocalLibraryBehaviour$' .)
    fi
elif [[ ${1-} == --qa-hw ]]; then
    (cd lark && LARK_QA_HW=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^(TestQASimSuite|TestQAHardwareSuite)$' .)
elif [[ ${1-} == --remote-ui ]]; then
    # Headless-Chromium DOM tests for the remote page. Dev-only and slower
    # than the fast path; never required for a plain ./scripts/check.sh.
    remote_ui_module=${LARK_PLAYWRIGHT_MODULE:-}
    if [[ -z $remote_ui_module ]]; then
        for candidate in /tmp/opencode/node_modules "${XDG_DATA_HOME:-$HOME/.local/share}/opencode/node_modules" "$HOME/.npm-global/lib/node_modules"; do
            for module in playwright playwright-core; do
                if [[ -d $candidate/$module ]]; then
                    remote_ui_module=$candidate/$module
                    break 2
                fi
            done
        done
    fi
    if [[ -z $remote_ui_module || ! -d $remote_ui_module ]]; then
        printf 'remote UI tests need playwright or playwright-core; set LARK_PLAYWRIGHT_MODULE to the module directory\n' >&2
        exit 2
    fi
    started=$(date +%s%3N)
    LARK_PLAYWRIGHT_MODULE=$remote_ui_module NODE_PATH=$(dirname -- "$remote_ui_module") \
        node --test lark/web/remote-ui/remote.test.mjs
    printf 'PASS remote UI tests (%s ms)\n' "$(( $(date +%s%3N) - started ))"
fi
