#!/usr/bin/env bash
# Build, vet, and run the checks plausibly affected by the diff against a base
# ref. This is the inner loop, never the gate: CI runs scripts/check.sh.
# See docs/testing.md.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

if (( $# > 1 )); then
    printf 'Usage: %s [ref]\n' "$0" >&2
    exit 2
fi

# shellcheck source=scripts/lib/timeit.sh
source scripts/lib/timeit.sh
# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
export CGO_ENABLED=1
# shellcheck source=scripts/lib/phases.sh
source scripts/lib/phases.sh

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

fast_mode() {
    local base merge f
    local -a changed=()
    local go=0 brain=0 shell=0 python=0 c_host=0 web=0

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
            lark/*) go=1 ;;
            brain/*) brain=1 ;;
            firmware/*) c_host=1 ;;
            web/*) web=1 ;;
            scripts/*.sh|scripts/lark) shell=1; python=1 ;;
            scripts/*) python=1 ;;
        esac
    done

    if ((go)); then
        timeit build phase_build
        timeit vet phase_vet
        timeit go-test phase_race
    else
        printf 'fast: skip Go (no lark/ change)\n'
    fi
    if ((brain)); then timeit brain phase_brain; else printf 'fast: skip brain tests (no brain/ change)\n'; fi
    if ((web)); then timeit web node --check web/remote/app.js; else printf 'fast: skip remote page syntax (no web/ change)\n'; fi
    if ((shell)); then timeit shell phase_shell; else printf 'fast: skip shell checks (no scripts/*.sh change)\n'; fi
    if ((python)); then timeit python phase_python; else printf 'fast: skip Python tests (scripts/ unchanged)\n'; fi
    if ((c_host)); then timeit c-host phase_c_host; else printf 'fast: skip host C tests (firmware unchanged)\n'; fi

    printf 'fast: subset only, not the gate; run ./scripts/check.sh before push\n'
}

fast_mode "${1-}"
time_summary
