#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

case "$#:${1-}" in
    0:|1:--remote-ui|1:--fast|2:--fast) ;;
    *) printf 'Usage: %s [--remote-ui | --fast [ref]]\n' "$0" >&2; exit 2 ;;
esac
mode=${1-}

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
    local view status path size magic
    if [[ -n $(git ls-files -- ':(glob)**/sdkconfig.local' ':(glob)**/lark.env') ]]; then
        printf 'sdkconfig.local and lark.env must not be tracked\n' >&2
        exit 1
    fi
    if [[ -n $(git ls-files -- ':(glob)brain/node_modules/**') ]]; then
        printf 'brain/node_modules must not be tracked\n' >&2
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
    # clone and bypass review.
    while IFS= read -r -d '' path; do
        size=$(wc -c <"$path")
        if ((size > 2 * 1024 * 1024)); then
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
    ./scripts/lint-shims.sh
    printf 'PASS tracked-file hygiene\n'
}

timeit shell phase_shell
timeit brain phase_brain
timeit python phase_python
timeit vet phase_vet
timeit go-race phase_race
timeit parakeet-compile phase_parakeet_compile
timeit c-host phase_c_host
timeit hygiene phase_hygiene
timeit diagrams phase_diagrams

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
        node --test web/remote-ui/remote.test.mjs
    printf 'PASS remote UI tests (%s ms)\n' "$(( $(date +%s%3N) - started ))"
}

case "$mode" in
    --remote-ui) timeit remote-ui phase_remote_ui ;;
esac

# Record the last green full gate for --fast's default base.
if [[ $# == 0 ]]; then
    git rev-parse HEAD > "$(git rev-parse --git-dir)/lark-check-green"
fi

time_summary
