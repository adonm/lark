#!/usr/bin/env python3
"""Write or update Lark's service environment file."""

import os
import re
import secrets
import sys
import tempfile
from pathlib import Path

# Retired when OpenCode became embedded in the brain process.
RETIRED = {"LARK_OPENCODE_URL", "LARK_OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD"}


def write_env_file(path, pin_gpu):
    path = Path(path)
    old = path.read_text() if path.exists() else (
        "LARK_HOST=0.0.0.0\nLARK_PORT=8765\nLARK_DEVICE_PORT=8766\n"
        f"LARK_TOKEN={secrets.token_hex(32)}\n"
    )
    settings = {}
    if not re.search(r"^LARK_CHATMAIL_RELAY=", old, re.M):
        settings["LARK_CHATMAIL_RELAY"] = "nine.testrun.org"
    if pin_gpu != "false":
        settings["GGML_VK_VISIBLE_DEVICES"] = pin_gpu
    kept = [line for line in old.splitlines(keepends=True)
            if line.strip().partition("=")[0].rstrip() not in settings.keys()
            and line.strip().partition("=")[0].rstrip() not in RETIRED]
    text = "".join(kept)
    if text and not text.endswith("\n"):
        text += "\n"
    text += "".join(f"{key}={value}\n" for key, value in settings.items())
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as f:
        try:
            f.write(text)
            f.close()
            os.replace(f.name, path)
        finally:
            Path(f.name).unlink(missing_ok=True)
    return text


def main(argv):
    write_env_file(argv[0], argv[1])


if __name__ == "__main__":
    main(sys.argv[1:])
