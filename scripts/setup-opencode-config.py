#!/usr/bin/env python3
"""Install Lark's OpenCode config without overwriting user model selections."""

import json
import os
import sys
import tempfile
from pathlib import Path

directory, lark_bin = map(Path, sys.argv[1:3])
source = Path(__file__).resolve().parent.parent / "config/opencode/lark-fallback.js"
context_source = Path(__file__).resolve().parent.parent / "config/opencode/lark-context/index.js"
config = directory / "opencode.jsonc"
package = "./lark-model-router"
context_package = "./lark-context"


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


def strip_json_comments(text, where=config):
    """Remove // and /* */ comments outside strings, as doctor.go does."""
    out = []
    i, n = 0, len(text)
    in_string = escaped = False
    while i < n:
        c = text[i]
        if in_string:
            out.append(c)
            if escaped:
                escaped = False
            elif c == "\\":
                escaped = True
            elif c == '"':
                in_string = False
            i += 1
        elif c == '"':
            in_string = True
            out.append(c)
            i += 1
        elif text.startswith("//", i):
            while i < n and text[i] != "\n":
                i += 1
            out.append("\n")
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            if end < 0:
                sys.exit(f"{where}: unterminated block comment")
            i = end + 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


def load(path):
    """Return the parsed config, or None when it does not exist yet."""
    try:
        raw = path.read_text()
    except FileNotFoundError:
        return None
    try:
        current = json.loads(strip_json_comments(raw, path))
    except ValueError as err:
        sys.exit(f"{path}: cannot parse JSONC: {err}")
    if not isinstance(current, dict):
        sys.exit(f"{path}: expected a JSON object")
    return current


def new_config():
    return {
        "$schema": "https://opencode.ai/config.json",
        "default_agent": "lark",
        "mcp": {"servers": {"lark": {"type": "local", "command": [str(lark_bin), "mcp"]}}},
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
        "plugins": [
            {"package": package, "options": {"fallback": FALLBACK_MODEL}},
            {"package": context_package},
        ],
    }


def section(current, key, kind):
    value = current.setdefault(key, kind())
    if not isinstance(value, kind):
        sys.exit(f"{config}: {key} must be a {kind.__name__}")
    return value


def migrate(current):
    """Move an untouched generated config off the retired gateway defaults."""
    agents = current.get("agents")
    plugins = current.get("plugins")
    if not isinstance(agents, dict) or not isinstance(plugins, list):
        return False
    fallback = None
    router = None
    for plugin in plugins:
        if isinstance(plugin, dict) and plugin.get("package") == package:
            router = plugin
            options = plugin.get("options")
            if isinstance(options, dict):
                fallback = options.get("fallback")
    if fallback != LEGACY_FALLBACK:
        return False
    if any(not isinstance(agents.get(name), dict) or agents[name].get("model") != model
           for name, model in LEGACY_MODELS.items()):
        return False
    agents["lark"]["model"] = PRIMARY_MODEL
    agents["deep"]["model"] = DEEP_MODEL
    router.setdefault("options", {})["fallback"] = FALLBACK_MODEL
    return True


def ensure_lark_mcp(current):
    """Keep default_agent and the local lark MCP server on every install."""
    changed = False
    if "default_agent" not in current:
        current["default_agent"] = "lark"
        changed = True
    mcp = section(current, "mcp", dict)
    servers = mcp.setdefault("servers", {})
    if not isinstance(servers, dict):
        sys.exit(f"{config}: mcp.servers must be an object")
    entry = servers.get("lark")
    if not isinstance(entry, dict):
        servers["lark"] = {"type": "local", "command": [str(lark_bin), "mcp"]}
        return True
    if entry.get("type") != "local":
        entry["type"] = "local"
        changed = True
    if entry.get("command") != [str(lark_bin), "mcp"]:
        entry["command"] = [str(lark_bin), "mcp"]
        changed = True
    return changed


def ensure_router_plugin(current):
    """Keep the model-router plugin, with its fallback option, installed."""
    plugins = section(current, "plugins", list)
    for plugin in plugins:
        if isinstance(plugin, dict) and plugin.get("package") == package:
            options = plugin.setdefault("options", {})
            if not isinstance(options, dict):
                sys.exit(f"{config}: plugin options must be an object")
            if "fallback" not in options:
                options["fallback"] = FALLBACK_MODEL
                return True
            return False
    plugins.append({"package": package, "options": {"fallback": FALLBACK_MODEL}})
    return True


def ensure_context_plugin(current):
    """Keep the request-side state stripper installed without touching other plugins."""
    plugins = section(current, "plugins", list)
    if any(isinstance(p, dict) and p.get("package") == context_package for p in plugins):
        return False
    plugins.append({"package": context_package})
    return True


def ensure_deep_guidance(current):
    """Add the local-library guidance without overwriting user selections."""
    agents = section(current, "agents", dict)
    deep = agents.setdefault("deep", {})
    if not isinstance(deep, dict):
        sys.exit(f"{config}: agents.deep must be an object")
    changed = False
    if "system" not in deep:
        deep["system"] = DEEP_SYSTEM
        changed = True
    if "permissions" not in deep:
        deep["permissions"] = DEEP_PERMISSIONS
        changed = True
    return changed


def verify(path):
    """Fail loudly when the written config lacks the entries Lark needs."""
    current = load(path)
    if current is None:
        sys.exit(f"{path}: installer did not write a config")
    plugins = current.get("plugins")
    packages = {p.get("package") for p in plugins if isinstance(p, dict)} if isinstance(plugins, list) else set()
    missing = [p for p in (package, context_package) if p not in packages]
    mcp = current.get("mcp")
    servers = mcp.get("servers") if isinstance(mcp, dict) else None
    if not isinstance(servers, dict) or not isinstance(servers.get("lark"), dict):
        missing.append("mcp.servers.lark")
    if not isinstance(current.get("default_agent"), str) or not current["default_agent"]:
        missing.append("default_agent")
    if missing:
        sys.exit(f"{path}: missing {', '.join(missing)} after install")


router = directory / package
router.mkdir(mode=0o700, parents=True, exist_ok=True)
put(router / "index.js", source.read_bytes())
context = directory / context_package
context.mkdir(mode=0o700, parents=True, exist_ok=True)
put(context / "index.js", context_source.read_bytes())

current = load(config)
if current is None:
    current = new_config()
    changed = True
else:
    changed = False
    changed |= migrate(current)
    changed |= ensure_lark_mcp(current)
    changed |= ensure_router_plugin(current)
    changed |= ensure_context_plugin(current)
    changed |= ensure_deep_guidance(current)
if changed:
    put(config, (json.dumps(current, indent=2) + "\n").encode())
verify(config)
