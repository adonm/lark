#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

case "$#:${1-}" in
    0:|1:--live|1:--e2e-real|1:--deployed|1:--real-model|1:--qa|1:--qa-hw|1:--remote-ui|1:--fast|2:--fast|1:--e2e-stub) ;;
    *) printf 'Usage: %s [--live | --e2e-real | --deployed | --real-model | --qa | --qa-hw | --remote-ui | --fast [ref] | --e2e-stub]\n' "$0" >&2; exit 2 ;;
esac
mode=${1-}

# The stub matrix is the fast development loop: one isolated e2e bridge and
# every non-hardware scenario. Skip the offline gate entirely.
if [[ $mode == --e2e-stub ]]; then
    exec ./scripts/e2e-stub.sh
fi

# shellcheck source=scripts/lib/timeit.sh
source scripts/lib/timeit.sh

# --fast lives in its own script so the gate stays small. It sources the same
# phases and timer as this one.
if [[ $mode == --fast ]]; then
    exec ./scripts/check-fast.sh "${2-}"
fi

# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
export CGO_ENABLED=1
# shellcheck source=scripts/lib/phases.sh
source scripts/lib/phases.sh

phase_hygiene() {
    local view status path size magic total
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
    # Tracked binaries and large blobs do not belong in git: they bloat every
    # clone and bypass review. Fixtures have their own cap below.
    total=0
    while IFS= read -r -d '' path; do
        size=$(wc -c <"$path")
        if [[ $path == lark/qa/fixtures/* ]]; then
            total=$((total + size))
        elif ((size > 2 * 1024 * 1024)); then
            printf 'tracked file %s is %d bytes; keep files over 2 MiB out of git\n' "$path" "$size" >&2
            exit 1
        fi
        magic=$(od -An -tx1 -N4 -- "$path" | tr -d ' \n')
        if [[ $magic == 7f454c46 || $magic == feedface || $magic == cefaedfe ||
              $magic == feedfacf || $magic == cffaedfe || $magic == cafebabe ||
              $magic == 4d5a* ]]; then
            printf 'tracked binary %s (magic %s) must not be committed\n' "$path" "$magic" >&2
            exit 1
        fi
    done < <(git ls-files -z)
    if ((total > 5 * 1024 * 1024)); then
        printf 'tracked fixtures are %d bytes; keep lark/qa/fixtures under 5 MiB\n' "$total" >&2
        exit 1
    fi
    ./scripts/lint-shims.sh
    printf 'PASS tracked-file hygiene\n'
}

timeit shell phase_shell
timeit node phase_node
timeit python phase_python
timeit vet phase_vet
timeit go-race phase_race
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

# --qa and --qa-hw deliberately target the installed server, so they opt in to
# its writes explicitly. Cleanup removes the rows each test created (see
# qaTempCompanion and localLibraryRemoveNewStories).
phase_qa() {
    (cd lark && LARK_QA_ALLOW_PRODUCTION=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQASimSuite$' .)
}

phase_qa_library() {
    (cd lark && LARK_QA_ALLOW_PRODUCTION=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^TestQALocalLibraryBehaviour$' .)
}

phase_qa_hw() {
    (cd lark && LARK_QA_ALLOW_PRODUCTION=1 LARK_QA_HW=1 "$GO_BIN" test -tags=live,sqlite_fts5 -v -count=1 -run '^(TestQASimSuite|TestQAHardwareSuite)$' .)
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

time_summary
