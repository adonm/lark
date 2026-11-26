#!/usr/bin/env bash
# Shared download verification. Source this after enabling `set -euo pipefail`.

# verify_sha256 CHECKSUM PATH
verify_sha256() {
    printf '%s  %s\n' "$1" "$2" | sha256sum --check --status
}

# download_verified URL DEST CHECKSUM
# Stages beside the destination, verifies, then renames into place. Returns
# non-zero without leaving a partial file when either step fails.
download_verified() {
    local url=$1 destination=$2 checksum=$3 staged
    staged=$(mktemp "${destination}.XXXXXXXX")
    if ! curl --fail --silent --show-error --location --output "$staged" "$url"; then
        rm -f -- "$staged"
        return 1
    fi
    if ! verify_sha256 "$checksum" "$staged"; then
        rm -f -- "$staged"
        return 1
    fi
    mv -fT -- "$staged" "$destination"
}
