#!/usr/bin/env python3
"""Install Lark's OpenCode config without overwriting user model selections."""

import json
import os
import sys
import tempfile
from pathlib import Path

directory, bridge = map(Path, sys.argv[1:3])
source = Path(__file__).resolve().parent.parent / "config/opencode/lark-fallback.js"
config = directory / "opencode.jsonc"
package = "./lark-model-router"


def put(path, content):
    if path.exists() and path.read_bytes() == content:
        return
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as tmp:
        try:
            tmp.write(content)
            tmp.close()
            os.replace(tmp.name, path)
        finally:
            Path(tmp.name).unlink(missing_ok=True)


PRIMARY_MODEL = "opencode-go/deepseek-v4.1-flash#low"
FALLBACK_MODEL = "opencode-go/glm-5.3-flash#low"
DEEP_MODEL = "opencode-go/glm-5.3#high"

router = directory / package
router.mkdir(mode=0o700, parents=True, exist_ok=True)
put(router / "index.js", source.read_bytes())
if not config.exists():
    update = {
        "$schema": "https://opencode.ai/config.json",
        "default_agent": "lark",
        "mcp": {"servers": {"lark": {"type": "local", "command": [str(bridge), "mcp"]}}},
        "agents": {
            "lark": {"model": PRIMARY_MODEL},
            "deep": {
                "model": DEEP_MODEL,
                "mode": "subagent",
                "description": "Delegate hard tasks: multi-step plans, stories, deep questions, or complex reasoning.",
            },
        },
        "plugins": [{"package": package, "options": {"fallback": FALLBACK_MODEL}}],
    }
    put(config, (json.dumps(update, indent=2) + "\n").encode())
