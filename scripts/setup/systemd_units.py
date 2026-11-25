#!/usr/bin/env python3
"""Render Lark's systemd user units from the checked-in templates."""

import re
import sys
from pathlib import Path

UNIT_NAMES = ("lark-edge.service", "lark-brain.service")


def escape(value):
    return value.translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "\n": "\\n", "\r": "\\r", "\t": "\\t",
    }))


def render_units(repo, data_home, edge_bin, node_bin, directory, env_file):
    repo, data_home, edge_bin, node_bin, directory, env_file = map(
        Path, (repo, data_home, edge_bin, node_bin, directory, env_file))
    units = directory
    paths = {"REPO_DIR": repo, "DATA_HOME": data_home, "EDGE_BIN": edge_bin,
             "NODE_BIN": node_bin, "ENV_FILE": env_file}
    rendered = {}
    for name in UNIT_NAMES:
        template = (repo / "lark" / name).read_text()
        text = re.sub(r"@([A-Z_]+)@",
                      lambda m: escape(str(paths[m[1]])), template)
        path = units / name
        path.write_text(text)
        path.chmod(0o644)
        rendered[name] = text
    return rendered


def main(argv):
    render_units(*argv)


if __name__ == "__main__":
    main(sys.argv[1:])
