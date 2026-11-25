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

# Symbols defined (or redefined) by the Go diff. Call sites and locals are not
# enough to justify a run; declarations are the honest signal.
fast_identifiers() {
    local out
    if ! out=$(git diff -U0 "$1" -- 'lark/*.go' |
        grep -E '^[+-][^+-]' |
        sed -nE \
            -e 's/^[+-][[:space:]]*func[[:space:]]+(\([^)]*\)[[:space:]]*)?([A-Za-z_][A-Za-z0-9_]*).*/\2/p' \
            -e 's/^[+-][[:space:]]*(type|var|const)[[:space:]]+([A-Za-z_][A-Za-z0-9_]*).*/\2/p' |
        awk 'BEGIN {
                 split("func string error int bool byte rune float64 struct interface any nil true false main init _ T E", a, " ")
                 for (i in a) skip[a[i]] = 1
             }
             !($1 in skip)' |
        sort -u); then
        out=
    fi
    [[ -z $out ]] || printf '%s\n' "$out"
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

phase_fast_go() {
    if [[ -n ${fast_pattern-} ]]; then
        (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 -run "^(${fast_pattern})$" .)
    else
        (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 .)
    fi
}

phase_fast_pkg() {
    (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 "./internal/$1")
}

# Subpackages (protospec, fonts/dogica) have their own suites; a top-level
# name filter would silently skip them.
phase_fast_all() { (cd lark && "$GO_BIN" test -race -tags sqlite_fts5 -count=1 ./...); }

fast_mode() {
    local base merge f stem camel name broad
    local -a changed=() ids=() names=() pkgs=()
    local -A tests=() pkg_seen=()
    local all_go=0 prod_go=0 node=0 c_host=0 shell=0 python=0

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
            lark/*)
                # lark/internal/<pkg> changes run that package's own suite.
                # Other subpackages are small enough that a change runs all Go.
                local rel=${f#lark/}
                if [[ $rel == internal/* ]]; then
                    local pkg=${rel#internal/}
                    pkg=${pkg%%/*}
                    if [[ -n $pkg && -z ${pkg_seen[$pkg]-} ]]; then
                        pkg_seen[$pkg]=1
                        pkgs+=("$pkg")
                    fi
                    continue
                fi
                case $rel in
                    *_test.go)
                        if [[ $rel == */* ]]; then
                            all_go=1
                        else
                            while IFS= read -r name; do tests[$name]="changed $rel"; done \
                                < <(grep -oE '^func (Test[A-Za-z0-9_]+)' "$f" | cut -d' ' -f2)
                        fi
                        ;;
                    *.go)
                        prod_go=1
                        if [[ $rel == */* ]]; then
                            all_go=1
                        else
                            stem=${rel%.go}
                            camel=$(sed -E 's/(^|_)([a-z])/\U\2/g' <<<"$stem")
                            while IFS= read -r name; do
                                tests[$name]="stem $rel"
                            done < <(grep -hoE "^func (Test[A-Za-z0-9_]*${camel}[A-Za-z0-9_]*)" lark/*_test.go |
                                     cut -d' ' -f2 | sort -u)
                        fi
                        ;;
                esac
                ;;
            config/opencode/*) node=1 ;;
            firmware/*) c_host=1 ;;
            scripts/*.sh) shell=1 ;;
            scripts/*) python=1 ;;
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
        printf 'fast: go.mod, a subpackage, or the module layout changed; running every Go test\n'
        timeit go-test-all phase_fast_all
    elif ((${#tests[@]})); then
        mapfile -t names < <(printf '%s\n' "${!tests[@]}" | sort)
        printf 'fast: %d related Go test(s)\n' "${#names[@]}"
        for name in "${names[@]}"; do
            printf 'fast:   %s (%s)\n' "$name" "${tests[$name]}"
        done
        fast_pattern=$(printf '%s\n' "${names[@]}" | paste -sd'|' -)
        timeit go-test phase_fast_go
    elif ((prod_go)); then
        # A production change that maps to no test name is a blind spot, not a
        # pass. Run the whole package and say why, loudly.
        printf '\nfast: WARNING production Go changed but no test mapped to it; running the package suite\n\n'
        fast_pattern=
        timeit go-test-package phase_fast_go
    else
        printf 'fast: no Go test names related to the change\n'
    fi

    if ((${#pkgs[@]})); then
        printf 'fast: %d internal package suite(s): %s\n' "${#pkgs[@]}" "${pkgs[*]}"
        for pkg in "${pkgs[@]}"; do
            timeit "go-test-$pkg" phase_fast_pkg "$pkg"
        done
    fi

    if ((shell)); then timeit shell phase_shell; else printf 'fast: skip shell checks (no scripts/*.sh change)\n'; fi
    if ((python)); then timeit python phase_python; else printf 'fast: skip Python tests (scripts/ unchanged)\n'; fi
    if ((node)); then timeit node phase_node; else printf 'fast: skip node tests (config/opencode unchanged)\n'; fi
    if ((c_host)); then timeit c-host phase_c_host; else printf 'fast: skip host C tests (firmware unchanged)\n'; fi

    printf 'fast: subset only, not the gate; run ./scripts/check.sh before push\n'
}

fast_mode "${1-}"
time_summary
