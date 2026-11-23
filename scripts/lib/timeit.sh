# shellcheck shell=bash
# Phase timing shared by scripts/check.sh and scripts/check-fast.sh.
timings=()
check_start=$(date +%s%3N)
timeit() {
    local name=$1 start ms
    shift
    start=$(date +%s%3N)
    "$@"
    ms=$(( $(date +%s%3N) - start ))
    timings+=("$name $(printf '%d.%01ds' $((ms / 1000)) $(((ms % 1000) / 100)))")
}
time_summary() {
    local ms=$(( $(date +%s%3N) - check_start ))
    timings+=("total $(printf '%d.%01ds' $((ms / 1000)) $(((ms % 1000) / 100)))")
    printf 'check: %s\n' "${timings[*]}"
}
