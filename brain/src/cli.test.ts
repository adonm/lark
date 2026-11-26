import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { envFileToken, token } from "./cli.ts";

function withEnv(name: string, value: string | undefined, run: () => void): void {
  const previous = process.env[name];
  if (value === undefined) delete process.env[name];
  else process.env[name] = value;
  try {
    run();
  } finally {
    if (previous === undefined) delete process.env[name];
    else process.env[name] = previous;
  }
}

test("envFileToken reads LARK_TOKEN and strips quotes", () => {
  const config = mkdtempSync(join(tmpdir(), "lark-cli-env-"));
  mkdirSync(join(config, "lark"));
  writeFileSync(
    join(config, "lark", "lark.env"),
    "LARK_HOST=0.0.0.0\nLARK_TOKEN='abc123'\nLARK_PORT=8765\n",
  );
  withEnv("XDG_CONFIG_HOME", config, () => {
    assert.equal(envFileToken(), "abc123");
  });
});

test("envFileToken tolerates a missing install", () => {
  const config = mkdtempSync(join(tmpdir(), "lark-cli-empty-"));
  withEnv("XDG_CONFIG_HOME", config, () => {
    assert.equal(envFileToken(), "");
  });
});

test("token prefers the process environment, then falls back to lark.env", () => {
  const root = mkdtempSync(join(tmpdir(), "lark-cli-token-"));
  const config = join(root, "config");
  const data = join(root, "data");
  mkdirSync(join(config, "lark"), { recursive: true });
  mkdirSync(data);
  writeFileSync(join(config, "lark", "lark.env"), "LARK_TOKEN=from-file\n");
  withEnv("XDG_CONFIG_HOME", config, () => {
    withEnv("LARK_DATA_DIR", data, () => {
      withEnv("LARK_TOKEN", undefined, () => assert.equal(token(), "from-file"));
      withEnv("LARK_TOKEN", "from-env", () => assert.equal(token(), "from-env"));
    });
  });
});
