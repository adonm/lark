#!/usr/bin/env bash
# Reject compatibility shims, vague deferrals, and error suppression. The
# repository policy is to fix the root cause or delete the code; see AGENTS.md.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

shim='legacy|backward(s)?[[:space:]]+compat|compatib|deprecat|migrat|retired|older server|\bv1\b|\bshim\b|echoear|@ts-ignore|@ts-expect-error|eslint-disable|\b(TODO|FIXME|HACK|XXX)\b|\bfor now\b|temporar(y|ily)[[:space:]]+(fix|hack|shim|workaround|patch|solution)|\bworkaround\b'
status=0

# Current uses of a flagged word that are not shims. Keep this list short and
# say why each line is allowed; delete an entry as soon as it stops matching.
allow=(
    'brain/src/e2e.test.ts:/v1|OpenCode HTTP API path'
    'brain/src/opencode.test.ts:/v1|OpenCode HTTP API path'
    'brain/src/e2e.test.ts:OpenAI-compatible|test model provider name'
    'brain/src/opencode.ts:OpenAI-compatible|provider name'
    'brain/src/opencode.ts:openai-compatible|provider package id'
    'scripts/uvx.sh:shim|uvx executable wrapper, not a compatibility shim'
)

filter_allowed() {
    local line entry prefix token reason allowed
    while IFS= read -r line; do
        [[ -z $line ]] && continue
        allowed=0
        for entry in "${allow[@]}"; do
            prefix=${entry%%:*}
            reason=${entry##*|}
            token=${entry#*:}
            token=${token%|*}
            if [[ $line == "$prefix:"* && $line == *"$token"* ]]; then
                printf 'allow: %s (%s)\n' "$line" "$reason" >&2
                allowed=1
                break
            fi
        done
        ((allowed)) || printf '%s\n' "$line"
    done
}

hits='' rc=0
raw=$(grep -rniE \
    --include='*.go' --include='*.ts' --include='*.js' --include='*.mjs' \
    --include='*.sh' --include='*.py' --include='*.md' \
    --include='*.c' --include='*.h' \
    --exclude-dir=node_modules --exclude-dir=managed_components \
    --exclude-dir=build --exclude-dir=build-box3 --exclude-dir=lark-upgrade \
    --exclude='lint-shims.sh' --exclude='*_gen.go' --exclude='*_gen.ts' \
    --exclude='proto.h' -- \
    "$shim" lark brain scripts web firmware/main firmware/tests agents .github) || rc=$?
if ((rc > 1)); then
    exit "$rc"
fi
hits=$(printf '%s\n' "$raw" | filter_allowed)
if [[ -n $hits ]]; then
    printf 'Shim markers found (fix or delete instead):\n%s\n' "$hits" >&2
    status=1
fi

suppressed='' rc=0
suppressed=$(grep -rnF --exclude='lint-shims.sh' -- '|| true' scripts) || rc=$?
if ((rc > 1)); then
    exit "$rc"
fi
if [[ -n $suppressed ]]; then
    printf 'Error suppression found in scripts (handle the status instead):\n%s\n' "$suppressed" >&2
    status=1
fi

nolint='' rc=0
nolint=$(grep -rnE --include='*.go' -- '\bnolint\b' lark) || rc=$?
if ((rc > 1)); then
    exit "$rc"
fi
if [[ -n $nolint ]]; then
    printf 'nolint directives found (fix the finding or justify it in issues):\n%s\n' "$nolint" >&2
    status=1
fi

if ((status == 0)); then
    printf 'PASS shim lint\n'
fi
exit "$status"
