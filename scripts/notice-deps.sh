#!/usr/bin/env bash
# Print the Go modules linked into the lark binary with their versions and
# detected licences. Regenerate the matching NOTICE list with this.
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../lark"

template='{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}'
go list -deps -tags sqlite_fts5 -f "$template" ./cmd/lark-edge | sort -u |
    while read -r module version; do
        dir=$(go list -m -f '{{.Dir}}' "$module")
        license=unknown
        if [[ -n $dir ]]; then
            for file in "$dir"/LICENSE* "$dir"/COPYING*; do
                [[ -f $file ]] || continue
                text=$(head -40 "$file")
                case $text in
                    *"Mozilla Public License"*) license=MPL-2.0 ;;
                    *"Apache License"*) license=Apache-2.0 ;;
                    *"MIT License"* | *"Permission is hereby granted, free of charge"*) license=MIT ;;
                    *"Redistribution and use in source and binary forms"*) license=BSD-3-Clause ;;
                esac
                break
            done
        fi
        printf '%s %s — %s https://%s\n' "$module" "$version" "$license" "$module"
    done
