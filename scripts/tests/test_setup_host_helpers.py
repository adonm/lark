"""Tests for the Python helpers setup-host.sh calls."""

import re
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "scripts" / "setup"))

import lark_env  # noqa: E402
import systemd_units  # noqa: E402


class LarkEnvTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.path = Path(self.tmp.name) / "lark.env"

    def test_new_file_gets_defaults_and_a_token(self):
        text = lark_env.write_env_file(self.path, "false")
        self.assertRegex(text, r"(?m)^LARK_TOKEN=[0-9a-f]{64}$")
        self.assertIn("LARK_HOST=0.0.0.0\n", text)
        self.assertIn("LARK_CHATMAIL_RELAY=nine.testrun.org\n", text)
        self.assertNotIn("GGML_VK_VISIBLE_DEVICES", text)
        self.assertNotIn("OPENCODE_SERVER_PASSWORD", text)

    def test_existing_values_are_kept(self):
        self.path.write_text(
            "LARK_HOST=127.0.0.1\n"
            "LARK_TOKEN=abc\n")
        text = lark_env.write_env_file(self.path, "1")
        self.assertIn("LARK_HOST=127.0.0.1\n", text)
        self.assertIn("LARK_TOKEN=abc\n", text)
        self.assertIn("GGML_VK_VISIBLE_DEVICES=1\n", text)


class SystemdUnitsTest(unittest.TestCase):
    def test_units_render_and_rewrite_custom_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            units = tmp / "units"
            units.mkdir()
            repo = tmp / "repo"
            (repo / "lark").mkdir(parents=True)
            for name in systemd_units.UNIT_NAMES:
                (repo / "lark" / name).write_text(
                    (REPO / "lark" / name).read_text())
            data_home = tmp / "data home"
            edge_bin = tmp / "bin" / "lark-edge"
            node_bin = tmp / "bin" / "node"
            env_file = tmp / "custom.env"
            rendered = systemd_units.render_units(
                repo, data_home, edge_bin, node_bin, units, env_file)
            self.assertEqual(set(rendered), set(systemd_units.UNIT_NAMES))
            for name, text in rendered.items():
                self.assertIsNone(re.search(r"@[A-Z_]+@", text), name)
                self.assertEqual((units / name).read_text(), text)
                self.assertEqual((units / name).stat().st_mode & 0o777, 0o644)
                self.assertIn(f"EnvironmentFile={env_file}", text)
                self.assertIn(f'Environment="LARK_DATA_DIR={data_home}/lark"', text)
            brain = rendered["lark-brain.service"]
            self.assertIn("After=network-online.target lark-edge.service", brain)
            self.assertIn("Wants=network-online.target lark-edge.service", brain)
            self.assertIn(f'ExecStart="{node_bin}" "{repo}/brain/src/main.ts"', brain)
            self.assertIn('Environment="MALLOC_ARENA_MAX=2"', brain)
            edge = rendered["lark-edge.service"]
            self.assertIn(f'ExecStart="{edge_bin}"', edge)
            self.assertIn('Environment="MALLOC_ARENA_MAX=2"', edge)


if __name__ == "__main__":
    unittest.main()
