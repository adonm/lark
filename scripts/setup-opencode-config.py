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


PRIMARY_MODEL = "opencode/claude-haiku-5-5#none"
FALLBACK_MODEL = "opencode/gemini-3.8-flash#low"
PREVIOUS_DEFAULT_MODEL = "opencode/gemini-4-hydrogen#low"
PREVIOUS_DEFAULT_FALLBACK = "opencode/claude-haiku-5-5"

router = directory / package
router.mkdir(mode=0o700, parents=True, exist_ok=True)
put(router / "index.js", source.read_bytes())
if not config.exists():
    update = {
        "$schema": "https://opencode.ai/config.json",
        "default_agent": "lark",
        "mcp": {"servers": {"lark": {"type": "local", "command": [str(bridge), "mcp"]}}},
        "agents": {"lark": {"model": PRIMARY_MODEL}},
        "plugins": [{"package": package, "options": {"fallback": FALLBACK_MODEL}}],
    }
    put(config, (json.dumps(update, indent=2) + "\n").encode())
else:
    try:
        current = json.loads(config.read_text())
    except Exception:
        current = None
    if isinstance(current, dict):
        changed = False
        agent = current.get("agents", {}).get("lark")
        if isinstance(agent, dict) and agent.get("model") == PREVIOUS_DEFAULT_MODEL:
            agent["model"] = PRIMARY_MODEL
            changed = True
        for plugin in current.get("plugins") or ():
            if isinstance(plugin, dict) and plugin.get("package") == package:
                opts = plugin.get("options")
                if isinstance(opts, dict) and opts.get("fallback") == PREVIOUS_DEFAULT_FALLBACK:
                    opts["fallback"] = FALLBACK_MODEL
                    changed = True
        if changed:
            put(config, (json.dumps(current, indent=2) + "\n").encode())
