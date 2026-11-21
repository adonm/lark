# shellcheck shell=bash
# Use mise explicitly when the uvx on PATH is an unconfigured shim.
run_uvx() {
    local uvx=""
    if command -v uvx >/dev/null 2>&1; then
        uvx=$(command -v uvx)
    fi
    if [[ "$uvx" == */mise/shims/uvx || -z "$uvx" ]] && command -v mise >/dev/null 2>&1; then
        mise exec uv@latest -- uvx "$@"
    elif [[ -n "$uvx" ]]; then
        "$uvx" "$@"
    else
        echo "uvx (from uv) is required." >&2
        return 127
    fi
}
