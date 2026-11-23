#!/usr/bin/env bash
# Reject compatibility shims, vague deferrals, and error suppression. The
# repository policy is to fix the root cause or delete the code; see AGENTS.md.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

shim='\b(TODO|FIXME|HACK|XXX)\b|\bfor now\b|temporar(y|ily)[[:space:]]+(fix|hack|shim|workaround|patch|solution)|\bworkaround\b|legacy fallback|Older server'
status=0
hits='' rc=0
hits=$(grep -rnE \
    --include='*.go' --include='*.js' --include='*.mjs' \
    --include='*.sh' --include='*.py' --include='*.md' \
    --exclude='lint-shims.sh' -- "$shim" lark config scripts) || rc=$?
if ((rc > 1)); then
    exit "$rc"
fi
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
