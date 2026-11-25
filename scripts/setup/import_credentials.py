#!/usr/bin/env python3
"""Copy OpenCode provider credentials into Lark's embedded server data."""

import json
import os
import sys
import tempfile
from pathlib import Path

# The brain's default primary, fallback and deep models all use this provider.
PROVIDERS = ("opencode",)


def import_auth(source, destination):
    """Merge missing provider entries from source auth.json into destination.

    Returns the number of providers added. Existing destination credentials are
    never overwritten, so a user can re-authenticate Lark separately.
    """
    source, destination = Path(source), Path(destination)
    if not source.is_file():
        return 0
    incoming = json.loads(source.read_text())
    if not isinstance(incoming, dict):
        raise ValueError("OpenCode auth.json is not an object")
    existing = {}
    if destination.is_file():
        current = json.loads(destination.read_text())
        if not isinstance(current, dict):
            raise ValueError("Lark auth.json is not an object")
        existing = current
    added = [name for name in PROVIDERS
             if name not in existing and name in incoming]
    if not added:
        if destination.is_file():
            destination.chmod(0o600)
        return 0
    existing.update({name: incoming[name] for name in added})
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temp = tempfile.mkstemp(dir=destination.parent, prefix=".auth.")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as handle:
            json.dump(existing, handle, indent=2)
            handle.write("\n")
        os.replace(temp, destination)
    finally:
        Path(temp).unlink(missing_ok=True)
    return len(added)


def main(argv):
    import_auth(*argv)


if __name__ == "__main__":
    main(sys.argv[1:])
