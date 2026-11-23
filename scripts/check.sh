#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

case "$#:${1-}" in
    0:|1:--live|1:--e2e-real|1:--deployed|1:--real-model|1:--qa|1:--qa-hw|1:--remote-ui|1:--fast|2:--fast|1:--no-race) ;;
    *) printf 'Usage: %s [--live | --e2e-real | --deployed | --real-model | --qa | --qa-hw | --remote-ui | --fast [ref] | --no-race]\n' "$0" >&2; exit 2 ;;
esac
mode=${1-}

check_start=$(date +%s%3N)
timings=()
timeit() {
    local name=$1 start ms
    shift
    start=$(date +%s%3N)
    "$@"
    ms=$(( $(date +%s%3N) - start ))
    timings+=("$name $(printf '%d.%01ds' $((ms / 1000)) $(((ms % 1000) / 100)))")
}
summary() {
    local ms=$(( $(date +%s%3N) - check_start ))
    timings+=("total $(printf '%d.%01ds' $((ms / 1000)) $(((ms % 1000) / 100)))")
    printf 'check: %s\n' "${timings[*]}"
}

host_tests=$(mktemp -d "${TMPDIR:-/tmp}/lark-check.XXXXXXXX")
trap 'rm -rf -- "$host_tests"' EXIT
host_cc=(gcc -std=c11 -Wall -Wextra -Werror -I firmware/main "-fsanitize=address,undefined"
         -fno-sanitize-recover=all -fno-pie -no-pie)

phase_shell() {
    local script
    for script in scripts/*.sh; do
        bash -n "$script"
    done
    if command -v shellcheck >/dev/null; then
        shellcheck -x scripts/*.sh
    fi
    printf 'PASS shell checks\n'
}

phase_node() { node --test config/opencode/*.test.mjs; }

phase_vet() {
    (
        cd lark
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

phase_hygiene() {
    local view status
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
}

# --fast: build + vet + the tests plausibly affected by the diff against a
# base ref. See docs/testing.md. Never the gate; CI runs the full script.
fast_base() {
    local green
    if [[ -n ${1-} ]]; then printf '%s\n' "$1"; return; fi
    if [[ -n ${LARK_FAST_BASE-} ]]; then printf '%s\n' "$LARK_FAST_BASE"; return; fi
    green=$(git rev-parse --git-dir)/lark-check-green
    if [[ -s $green && $(<"$green") =~ ^[0-9a-f]{40}$ ]] &&
       git merge-base --is-ancestor "$(<"$green")" HEAD 2>/dev/null; then
        printf '%s\n' "$(<"$green")"
        return
    fi
    printf 'origin/main\n'
}

# Symbols defined (or redefined) by the Go diff. Call sites and locals are not
# enough to justify a run; declarations are the honest signal.
fast_identifiers() {
    git diff -U0 "$1" -- 'lark/*.go' |
        grep -E '^[+-][^+-]' |
        sed -nE \
            -e 's/^[+-][[:space:]]*func[[:space:]]+(\([^)]*\)[[:space:]]*)?([A-Za-z_][A-Za-z0-9_]*).*/\2/p' \
            -e 's/^[+-][[:space:]]*(type|var|const)[[:space:]]+([A-Za-z_][A-Za-z0-9_]*).*/\2/p' |
        grep -vxE 'func|string|error|int|bool|byte|rune|float64|struct|interface|any|nil|true|false|main|init|_|T|E' |
        sort -u || true
}

# Tests whose bodies mention a symbol defined in the diff: TestName<TAB>symbol.
fast_symbol_tests() {
    awk -v idfile="$1" '
        BEGIN { n = 0; while ((getline w < idfile) > 0) ids[++n] = w }
        function flush(  i) {
            if (name == "") return
            for (i = 1; i <= n; i++)
                if (body ~ ("(^|[^A-Za-z0-9_])" ids[i] "([^A-Za-z0-9_]|$)"))
                    print name "\t" ids[i]
            name = ""
        }
        /^func / {
            flush()
            body = $0
            if ($0 ~ /^func Test[A-Za-z0-9_]+\(/) {
                name = $2
                sub(/\(.*/, "", name)
            } else {
                name = ""
            }
            next
        }
        name != "" { body = body " " $0 }
        END { flush() }
    ' lark/*_test.go
}

phase_build() { (cd lark && "$GO_BIN" build -tags sqlite_fts5 ./...); }

phase_fast_go() {
    if [[ -n ${fast_pattern-} ]]; then
        (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 -run "^(${fast_pattern})$" .)
    else
        (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 .)
    fi
}

fast_mode() {
    local base merge f stem camel name broad
    local -a changed=() ids=() names=()
    local -A tests=()
    local all_go=0 node=0 c_host=0 shell=0

    base=$(fast_base "${1-}")
    merge=$(git merge-base "$base" HEAD) || {
        printf 'fast: cannot find a merge base for %s\n' "$base" >&2
        exit 2
    }
    mapfile -t changed < <({ git diff --name-only "$merge" -- .; git ls-files --others --exclude-standard; } | sort -u)
    printf 'fast: %d changed file(s) since %s (%.12s)\n' "${#changed[@]}" "$base" "$merge"
    if ((${#changed[@]} == 0)); then
        printf 'fast: nothing changed; run ./scripts/check.sh before any push\n'
        return
    fi

    for f in "${changed[@]}"; do
        case $f in
            lark/go.mod|lark/go.sum) all_go=1 ;;
            lark/*_test.go)
                while IFS= read -r name; do tests[$name]="changed ${f##*/}"; done \
                    < <(grep -oE '^func (Test[A-Za-z0-9_]+)' "$f" | cut -d' ' -f2)
                ;;
            lark/*.go)
                stem=${f##*/}
                stem=${stem%.go}
                camel=$(sed -E 's/(^|_)([a-z])/\U\2/g' <<<"$stem")
                while IFS= read -r name; do
                    tests[$name]="stem ${stem}.go"
                done < <(grep -hoE "^func (Test[A-Za-z0-9_]*${camel}[A-Za-z0-9_]*)" lark/*_test.go |
                         cut -d' ' -f2 | sort -u)
                ;;
            config/opencode/*) node=1 ;;
            firmware/*) c_host=1 ;;
            scripts/*.sh) shell=1 ;;
        esac
    done

    if ! ((all_go)) && ((${#changed[@]})); then
        mapfile -t ids < <(fast_identifiers "$merge")
        if ((${#ids[@]})); then
            printf '%s\n' "${ids[@]}" > "$host_tests/idents"
            fast_symbol_tests "$host_tests/idents" > "$host_tests/refs"
            # A symbol used by many tests is a common word (start, status,
            # c), not a signal. Keep symbols that pin at most 4 tests.
            awk -F'\t' '{ n[$2]++ } END { for (i in n) if (n[i] <= 4) print i }' \
                "$host_tests/refs" | sort > "$host_tests/keep"
            awk -F'\t' 'NR == FNR { keep[$1] = 1; next } ($2 in keep)' \
                "$host_tests/keep" "$host_tests/refs" > "$host_tests/picked"
            while IFS=$'\t' read -r name id; do
                [[ -n $name ]] && tests[$name]="references $id"
            done < "$host_tests/picked"
            broad=$(awk -F'\t' '{ n[$2]++ } END { c = 0; for (i in n) if (n[i] > 4) c++; print c }' "$host_tests/refs")
            if ((broad > 0)); then
                printf 'fast: ignored %d common symbol(s) referenced by more than 4 tests\n' "$broad"
            fi
        fi
    fi

    timeit build phase_build
    timeit vet phase_vet

    if ((all_go)); then
        printf 'fast: go.mod/go.sum changed; running every Go test\n'
        fast_pattern=
        timeit go-test-all phase_fast_go
    elif ((${#tests[@]})); then
        mapfile -t names < <(printf '%s\n' "${!tests[@]}" | sort)
        printf 'fast: %d related Go test(s)\n' "${#names[@]}"
        for name in "${names[@]}"; do
            printf 'fast:   %s (%s)\n' "$name" "${tests[$name]}"
        done
        fast_pattern=$(printf '%s\n' "${names[@]}" | paste -sd'|' -)
        timeit go-test phase_fast_go
    else
        printf 'fast: no Go test names related to the change\n'
    fi

    if ((shell)); then timeit shell phase_shell; else printf 'fast: skip shell checks (no scripts/*.sh change)\n'; fi
    if ((node)); then timeit node phase_node; else printf 'fast: skip node tests (config/opencode unchanged)\n'; fi
    if ((c_host)); then timeit c-host phase_c_host; else printf 'fast: skip host C tests (firmware unchanged)\n'; fi

    printf 'fast: subset only, not the gate; run ./scripts/check.sh before push\n'
}

if [[ $mode == --fast ]]; then
    # shellcheck source=scripts/go-bin.sh
    source scripts/go-bin.sh
    GO_BIN=$(resolve_go)
    export CGO_ENABLED=1
    fast_mode "${2-}"
    summary
    exit 0
fi

timeit shell phase_shell

# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
export CGO_ENABLED=1

timeit node phase_node
timeit vet phase_vet
if [[ $mode == --no-race ]]; then
    # CI splits the race suite across jobs; the offline job keeps vet and the
    # live-tag compile so the workflow still covers every build tag.
    printf 'SKIP go-race (--no-race; CI shards run it)\n'
else
    timeit go-race phase_race
fi
timeit go-live-compile phase_live_compile
timeit c-host phase_c_host
timeit hygiene phase_hygiene

phase_live() {
    local native data_home model test_bin
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
        case "$mode" in
            --live) LARK_TEST_BIN="$test_bin" LARK_E2E_MODE=stub "$GO_BIN" test -tags=live -v -count=1 -run '^TestVirtualE2E$' . ;;
            --e2e-real) LARK_TEST_BIN="$test_bin" LARK_E2E_MODE=real "$GO_BIN" test -tags=live -v -count=1 -run '^TestVirtualE2E$' . ;;
            # Read-only installed-service and OTA checks. Never uploads or flashes.
            --deployed) LARK_TEST_BIN="$test_bin" "$GO_BIN" test -tags=live -v -count=1 -run '^TestLive' . ;;
        esac
    )
}

phase_real_model() {
    (cd lark && LARK_REAL_MODEL=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestRealModelLarkCodeMode$' .)
}

phase_qa() { (cd lark && "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQASimSuite$' .); }

phase_qa_library() { (cd lark && "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQALocalLibraryBehaviour$' .); }

phase_qa_hw() {
    (cd lark && LARK_QA_HW=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^(TestQASimSuite|TestQAHardwareSuite)$' .)
}

phase_remote_ui() {
    # Headless-Chromium DOM tests for the remote page. Dev-only and slower
    # than the fast path; never required for a plain ./scripts/check.sh.
    local remote_ui_module candidate module started
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
}

case "$mode" in
    --live|--e2e-real|--deployed) timeit "${mode#--}" phase_live ;;
    --real-model) timeit real-model phase_real_model ;;
    --qa)
        timeit qa phase_qa
        if [[ ${LARK_QA_LIBRARY-} == 1 ]]; then timeit qa-library phase_qa_library; fi
        ;;
    --qa-hw) timeit qa-hw phase_qa_hw ;;
    --remote-ui) timeit remote-ui phase_remote_ui ;;
esac

# Record the last green full gate for --fast's default base.
if [[ $# == 0 ]]; then
    git rev-parse HEAD > "$(git rev-parse --git-dir)/lark-check-green"
fi

summary
