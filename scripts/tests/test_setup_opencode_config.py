"""Tests for scripts/setup-opencode-config.py."""

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SCRIPT = REPO / "scripts" / "setup-opencode-config.py"
LARK_BIN = "/home/dev/.local/share/lark/lark"


def strip_comments(text):
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
        elif text.startswith("/*", i):
            end = text.find("*/", i + 2)
            i = n if end < 0 else end + 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


class SetupOpenCodeConfigTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.config = self.dir / "opencode.jsonc"
        self.addCleanup(self.tmp.cleanup)

    def run_installer(self, expect=0):
        proc = subprocess.run(
            [sys.executable, str(SCRIPT), str(self.dir), LARK_BIN],
            capture_output=True, text=True)
        self.assertEqual(proc.returncode, expect, proc.stdout + proc.stderr)
        return proc

    def read(self):
        return json.loads(strip_comments(self.config.read_text()))

    def test_first_install_writes_required_entries(self):
        self.run_installer()
        cfg = self.read()
        self.assertEqual(cfg["default_agent"], "lark")
        self.assertEqual(cfg["mcp"]["servers"]["lark"]["command"], [LARK_BIN, "mcp"])
        packages = [p["package"] for p in cfg["plugins"]]
        self.assertIn("./lark-model-router", packages)
        self.assertIn("./lark-context", packages)
        self.assertTrue((self.dir / "./lark-context/index.js").exists())

    def test_commented_jsonc_is_parsed_and_completed(self):
        self.config.write_text(
            "{\n"
            "  // keep the user's model choice\n"
            "  \"agents\": {\"lark\": {\"model\": \"custom/model\"}},\n"
            "  /* an empty plugin list */\n"
            "  \"plugins\": []\n"
            "}\n")
        self.run_installer()
        cfg = self.read()
        self.assertEqual(cfg["agents"]["lark"]["model"], "custom/model")
        packages = {p["package"] for p in cfg["plugins"]}
        self.assertEqual(packages, {"./lark-model-router", "./lark-context"})
        self.assertEqual(cfg["default_agent"], "lark")
        self.assertIn("lark", cfg["mcp"]["servers"])

    def test_existing_config_missing_entries_is_completed(self):
        self.config.write_text(json.dumps({
            "agents": {"deep": {"system": "keep this"}},
            "plugins": [{"package": "other"}],
        }))
        self.run_installer()
        cfg = self.read()
        self.assertEqual(cfg["agents"]["deep"]["system"], "keep this")
        self.assertTrue(cfg["agents"]["deep"]["permissions"])
        self.assertEqual(cfg["default_agent"], "lark")
        self.assertEqual(cfg["mcp"]["servers"]["lark"]["type"], "local")
        packages = {p["package"] for p in cfg["plugins"]}
        self.assertEqual(packages, {"other", "./lark-model-router", "./lark-context"})

    def test_second_run_is_byte_identical(self):
        self.run_installer()
        first = self.config.read_bytes()
        self.run_installer()
        self.assertEqual(self.config.read_bytes(), first)

    def test_retired_gateway_defaults_migrate(self):
        self.config.write_text(json.dumps({
            "agents": {
                "lark": {"model": "opencode-go/deepseek-v4.1-flash#low"},
                "deep": {"model": "opencode-go/glm-5.3#high"},
            },
            "plugins": [{"package": "./lark-model-router",
                         "options": {"fallback": "opencode-go/glm-5.3-flash#low"}}],
        }))
        self.run_installer()
        cfg = self.read()
        self.assertEqual(cfg["agents"]["lark"]["model"], "opencode/deepseek-v4.1-flash#low")
        self.assertEqual(cfg["agents"]["deep"]["model"], "opencode/glm-5.3#high")

    def test_invalid_jsonc_fails_loudly(self):
        self.config.write_text('{"plugins": [')
        proc = self.run_installer(expect=1)
        self.assertIn("cannot parse JSONC", proc.stderr)


if __name__ == "__main__":
    unittest.main()
