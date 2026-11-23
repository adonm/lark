#!/usr/bin/env bash
# Fast end-to-end QA: one stub bridge (scripted STT/TTS/Delta Chat) and the
# full lark sim scenario matrix. Builds the product with the e2e tag, so no
# Parakeet, Silero, Kokoro, OpenCode or Delta Chat installation is needed.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
export CGO_ENABLED=1

test_bin=$(mktemp -d "${TMPDIR:-/tmp}/lark-e2e-stub.XXXXXXXX")
trap 'rm -rf -- "$test_bin"' EXIT

(
    cd lark
    "$GO_BIN" build -tags e2e,sqlite_fts5 -o "$test_bin/lark" .
)
started=$(date +%s%3N)
set +e
(
    cd lark
    LARK_TEST_BIN="$test_bin/lark" LARK_E2E_MODE=stub \
        "$GO_BIN" test -tags live,sqlite_fts5 -v -count=1 -timeout 120s -run '^TestQAStubSuite$' . \
        >"$test_bin/log" 2>&1
)
status=$?
set -e
elapsed=$(( $(date +%s%3N) - started ))

awk -v root="TestQAStubSuite/" '
    /^ *--- (PASS|FAIL|SKIP): / {
        verdict = $2; name = $3; ms = $4
        gsub(/[()]/, "", ms)
        sub(root, "", name)
        if (name == "" || name == "TestQAStubSuite" || name ~ /\//) next
        printf "%-22s %-5s %s\n", name, verdict, ms
    }
' "$test_bin/log" || true
printf '%-22s %-5s %s\n' "TOTAL" "$([[ $status == 0 ]] && echo PASS || echo FAIL)" "$(( elapsed / 1000 )).$(( elapsed % 1000 / 100))s"

if [[ $status != 0 ]]; then
    printf '\n--- go test output ---\n'
    cat "$test_bin/log"
fi
exit "$status"
