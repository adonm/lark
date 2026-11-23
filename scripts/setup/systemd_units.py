#!/usr/bin/env python3
"""Render Lark's systemd user units from the checked-in templates."""

import re
import sys
from pathlib import Path


def escape(value):
    return value.translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "\n": "\\n", "\r": "\\r", "\t": "\\t",
    }))


def render_units(repo, data_home, binary, directory, opencode, env_file, oc_root):
    repo, data, binary, directory, opencode, env_file, oc_root = map(
        Path, (repo, data_home, binary, directory, opencode, env_file, oc_root))
    units = directory
    paths = {"REPO_DIR": repo, "DATA_HOME": data,
             "LARK_BIN": binary, "OPENCODE_BIN": opencode, "ENV_FILE": env_file}
    rendered = {}
    for name in ("lark.service", "lark-opencode.service"):
        template = (repo / "lark" / name).read_text()
        text = re.sub(r"@([A-Z_]+)@",
                      lambda m: escape(str(paths[m[1]])), template)
        if name == "lark-opencode.service":
            if oc_root != Path.home() / ".local/share/lark/opencode":
                text = text.replace("%h/.local/share/lark/opencode", escape(str(oc_root)))
                text = text.replace(
                    f"WorkingDirectory={escape(str(oc_root))}/workspace",
                    f'WorkingDirectory="{escape(str(oc_root))}/workspace"')
            if env_file != Path.home() / ".config/lark/lark.env":
                text = text.replace("EnvironmentFile=%h/.config/lark/lark.env",
                                    f"EnvironmentFile={escape(str(env_file))}")
        path = units / name
        path.write_text(text)
        path.chmod(0o644)
        rendered[name] = text
    return rendered


def main(argv):
    render_units(*argv)


if __name__ == "__main__":
    main(sys.argv[1:])
