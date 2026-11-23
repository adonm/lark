"""Tests for the Python helpers setup-host.sh calls."""

import re
import sys
import tempfile
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "scripts" / "setup"))

import import_credentials  # noqa: E402
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

    def test_existing_values_are_kept_and_settings_replaced_once(self):
        self.path.write_text(
            "LARK_HOST=127.0.0.1\n"
            "LARK_TOKEN=abc\n"
            "LARK_OPENCODE_PASSWORD=" + "a" * 64 + "\n"
            "LARK_OPENCODE_URL=http://stale:1\n")
        text = lark_env.write_env_file(self.path, "1")
        self.assertIn("LARK_HOST=127.0.0.1\n", text)
        self.assertIn("LARK_TOKEN=abc\n", text)
        self.assertEqual(text.count("LARK_OPENCODE_PASSWORD="), 1)
        self.assertIn("LARK_OPENCODE_PASSWORD=" + "a" * 64 + "\n", text)
        self.assertEqual(text.count("LARK_OPENCODE_URL="), 1)
        self.assertIn("LARK_OPENCODE_URL=http://127.0.0.1:49375\n", text)
        self.assertIn("GGML_VK_VISIBLE_DEVICES=1\n", text)


class SystemdUnitsTest(unittest.TestCase):
    def test_units_render_and_rewrite_custom_paths(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            units = tmp / "units"
            units.mkdir()
            repo = tmp / "repo"
            (repo / "lark").mkdir(parents=True)
            for name in ("lark.service", "lark-opencode.service"):
                (repo / "lark" / name).write_text(
                    (REPO / "lark" / name).read_text())
            oc_root = tmp / "elsewhere" / "opencode"
            env_file = tmp / "custom.env"
            rendered = systemd_units.render_units(
                repo, tmp / "data", tmp / "bin" / "lark", units,
                tmp / "bin" / "opencode", env_file, oc_root)
            for name, text in rendered.items():
                self.assertIsNone(re.search(r"@[A-Z_]+@", text), name)
                self.assertEqual((units / name).read_text(), text)
                self.assertEqual((units / name).stat().st_mode & 0o777, 0o644)
            opencode = rendered["lark-opencode.service"]
            self.assertIn(f'WorkingDirectory="{oc_root}/workspace"', opencode)
            self.assertIn(f"EnvironmentFile={env_file}", opencode)
            self.assertNotIn("%h/.local/share/lark/opencode", opencode)


class ImportCredentialsTest(unittest.TestCase):
    def test_reads_url_and_password(self):
        url, password = import_credentials.lark_settings(
            "# comment\nLARK_OPENCODE_URL=http://127.0.0.1:49375\n"
            "LARK_OPENCODE_PASSWORD=" + "b" * 64 + "\n")
        self.assertEqual(url, "http://127.0.0.1:49375")
        self.assertEqual(password, "b" * 64)


if __name__ == "__main__":
    unittest.main()
