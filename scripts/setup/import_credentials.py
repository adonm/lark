#!/usr/bin/env python3
"""Import Lark's provider credentials into the dedicated OpenCode server."""

import base64
import json
import os
import re
import subprocess
import sys
import time
import urllib.request
from pathlib import Path


def lark_settings(env_text):
    """Return the OpenCode URL and password from the env file."""
    settings = dict(re.findall(r"^\s*(LARK_OPENCODE_\w+)\s*=([^\n]*)", env_text, re.M))
    return settings["LARK_OPENCODE_URL"], settings["LARK_OPENCODE_PASSWORD"]


def import_credentials(env_file, root, opencode):
    root = Path(root)
    url, password = lark_settings(Path(env_file).read_text())
    auth = base64.b64encode(f"opencode:{password}".encode()).decode()
    client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def get(path):
        request = urllib.request.Request(
            f"{url}{path}", headers={"Authorization": f"Basic {auth}"})
        with client.open(request, timeout=5) as response:
            return json.load(response)

    deadline = time.monotonic() + 10
    while True:
        try:
            agent = get("/api/agent/lark")
            break
        except OSError:
            if time.monotonic() >= deadline:
                sys.exit("Dedicated OpenCode API did not become ready with Basic auth")
            time.sleep(0.2)

    try:
        primary = agent["data"]["model"]["providerID"]
        target_config = Path(root, "config/opencode/opencode.jsonc").resolve()
        router = next(p for entry in get("/api/config")
                      if entry.get("path") and Path(entry["path"]).resolve() == target_config
                      for p in entry["info"].get("plugins") or ()
                      if isinstance(p, dict) and p.get("package") == "./lark-model-router")
        fallback = router["options"]["fallback"].split("/", 1)[0]
    except (OSError, KeyError, StopIteration, TypeError, ValueError) as err:
        sys.exit(f"Could not read the Lark model settings from the dedicated OpenCode config: {err}")
    for provider in sorted({primary, fallback}):
        if not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*", provider):
            sys.exit("Invalid Lark model provider")
        marker = Path(root, "state", f"imported-provider-{provider}")
        if not marker.exists():
            try:
                credentials = subprocess.run([opencode, "auth", "export", provider], check=True,
                                             capture_output=True, timeout=20).stdout
                if json.loads(credentials):
                    subprocess.run([opencode, "--server", url, "auth", "import"], input=credentials,
                                   env={**os.environ, "OPENCODE_SERVER_PASSWORD": password}, check=True,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
                    marker.touch()
            except (OSError, ValueError, subprocess.SubprocessError):
                sys.exit("Could not import the configured providers' OpenCode credentials")


def main(argv):
    import_credentials(*argv)


if __name__ == "__main__":
    main(sys.argv[1:])
