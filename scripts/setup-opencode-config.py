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


PRIMARY_MODEL = "opencode/deepseek-v4.1-flash#low"
FALLBACK_MODEL = "opencode/glm-5.3-flash#low"
DEEP_MODEL = "opencode/glm-5.3#high"
# Earlier releases defaulted to the opencode-go gateway. Rewrite only an
# untouched generated config so installs keep deliberate model selections.
LEGACY_MODELS = {
    "lark": "opencode-go/deepseek-v4.1-flash#low",
    "deep": "opencode-go/glm-5.3#high",
}
LEGACY_FALLBACK = "opencode-go/glm-5.3-flash#low"
DEEP_SYSTEM = (
    "Research and draft for a voice companion. Use the local library for grounded facts: "
    "search first, read bounded sections, never paste long text. Keep answers short and plain, "
    "with no Markdown or formal citations. For stories, import a fitting local text and retell "
    "it at kid length."
)
DEEP_PERMISSIONS = [
    {"action": "*", "resource": "*", "effect": "deny"},
    {"action": "lark_*", "resource": "*", "effect": "allow"},
    {"action": "websearch", "resource": "*", "effect": "allow"},
]


def migrate(path):
    try:
        current = json.loads(path.read_text())
        agents = current["agents"]
        plugins = current["plugins"]
        fallback = next(p["options"]["fallback"] for p in plugins
                        if isinstance(p, dict) and p.get("package") == package)
    except (OSError, ValueError, KeyError, StopIteration, TypeError, AttributeError):
        return
    if any(agents.get(name, {}).get("model") != model for name, model in LEGACY_MODELS.items()):
        return
    if fallback != LEGACY_FALLBACK:
        return
    agents["lark"]["model"] = PRIMARY_MODEL
    agents["deep"]["model"] = DEEP_MODEL
    for plugin in plugins:
        if isinstance(plugin, dict) and plugin.get("package") == package:
            plugin["options"]["fallback"] = FALLBACK_MODEL
    put(path, (json.dumps(current, indent=2) + "\n").encode())


def ensure_deep_guidance(path):
    """Add the local-library guidance without overwriting user selections."""
    try:
        current = json.loads(path.read_text())
        deep = current.setdefault("agents", {}).setdefault("deep", {})
    except (OSError, ValueError, TypeError, AttributeError):
        return
    before = json.dumps(current, sort_keys=True)
    deep.setdefault("system", DEEP_SYSTEM)
    deep.setdefault("permissions", DEEP_PERMISSIONS)
    if json.dumps(current, sort_keys=True) != before:
        put(path, (json.dumps(current, indent=2) + "\n").encode())


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
                "system": DEEP_SYSTEM,
                "permissions": DEEP_PERMISSIONS,
            },
        },
        "plugins": [{"package": package, "options": {"fallback": FALLBACK_MODEL}}],
    }
    put(config, (json.dumps(update, indent=2) + "\n").encode())
else:
    migrate(config)
ensure_deep_guidance(config)
