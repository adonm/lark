import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { Router } from "./router.ts";
import { CaptureStore } from "./capture.ts";
import { RemoteManager } from "./remote.ts";
import { startHttp, type RunningHttp } from "./http.ts";
import { FakeBrain, FakeEdge } from "./testfakes.ts";
import type { EdgeClient } from "./edge_client.ts";

interface Server {
  http: RunningHttp;
  base: string;
  dir: string;
  db: LarkDb;
  edge: FakeEdge;
  token: string;
}

async function server(): Promise<Server> {
  const dir = mkdtempSync(join(tmpdir(), "lark-http-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", form: "a chameleon", configured: true });
  const edge = new FakeEdge();
  const router = new Router({ db, edge, brain: new FakeBrain(), dataDir: dir, log: () => undefined });
  const remote = new RemoteManager({ db, localPort: 8765, runner: async () => ({ stdout: "", stderr: "", code: 0 }) });
  const captures = new CaptureStore({
    dir: join(dir, "debug"),
    edge: edge as unknown as EdgeClient,
    terminalForCompanion: () => undefined,
    companionForTerminal: () => undefined,
  });
  const http = await startHttp({
    host: "127.0.0.1",
    port: 0,
    adminToken: "admin",
    dataDir: dir,
    db,
    edge: edge as unknown as EdgeClient,
    router,
    remote,
    captures,
    libraryDir: join(dir, "library"),
  });
  const token = (await remote.issueLocal("greg", "sam@example.org", "Sam")).split("token=")[1] ?? "";
  return { http, base: `http://127.0.0.1:${http.port}`, dir, db, edge, token };
}

const ADMIN = { "x-lark-token": "admin" };

test("OTA files are admin-only and refuse traversal and non-GET", async (t) => {
  const s = await server();
  t.after(async () => {
    await s.http.close();
  });
  const dir = join(s.dir, "ota");
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "fw.bin"), Buffer.from("firmware"));
  writeFileSync(join(dir, "manifest.json"), "{}");

  assert.equal((await fetch(`${s.base}/ota/fw.bin`)).status, 401);
  const bin = await fetch(`${s.base}/ota/fw.bin`, { headers: ADMIN });
  assert.equal(bin.status, 200);
  assert.equal(bin.headers.get("content-type"), "application/octet-stream");
  assert.equal(await bin.text(), "firmware");
  const manifest = await fetch(`${s.base}/ota/manifest.json`, { headers: ADMIN });
  assert.equal(manifest.headers.get("content-type"), "application/json; charset=utf-8");

  assert.equal((await fetch(`${s.base}/ota/missing.bin`, { headers: ADMIN })).status, 404);
  assert.equal((await fetch(`${s.base}/ota/fw.bin`, { method: "POST", headers: ADMIN })).status, 404);
  const traversal = await fetch(`${s.base}/ota/%2e%2e%2flark.db`, { headers: ADMIN });
  assert.equal(traversal.status, 404);
});

test("admin routes reject bad auth, methods and payloads", async (t) => {
  const s = await server();
  t.after(async () => {
    await s.http.close();
  });
  assert.equal((await fetch(`${s.base}/api/companions`, { headers: { "x-lark-token": "wrong" } })).status, 401);
  assert.equal((await fetch(`${s.base}/api/unknown`, { headers: ADMIN })).status, 404);
  assert.equal((await fetch(`${s.base}/api/companions/greg`, { method: "POST", headers: ADMIN })).status, 404);
  assert.equal(
    (await fetch(`${s.base}/api/companions`, { method: "POST", headers: ADMIN, body: "{" })).status,
    400,
  );
  assert.equal(
    (await fetch(`${s.base}/api/companions`, { method: "POST", headers: ADMIN, body: JSON.stringify({ name: "" }) })).status,
    400,
  );
  assert.equal(
    (await fetch(`${s.base}/api/companions`, { method: "POST", headers: ADMIN, body: JSON.stringify({ name: "Greg" }) })).status,
    409,
  );
  assert.equal((await fetch(`${s.base}/api/companions/No%20Way`, { headers: ADMIN })).status, 400);
  assert.equal(
    (await fetch(`${s.base}/api/companions/greg`, { method: "PATCH", headers: ADMIN, body: JSON.stringify({ listening: "yes" }) })).status,
    400,
  );
  const patched = await fetch(`${s.base}/api/companions/greg`, {
    method: "PATCH",
    headers: ADMIN,
    body: JSON.stringify({ listening: true }),
  });
  assert.equal(patched.status, 200);
  assert.equal(s.db.getCompanion("greg")?.listening, true);
});

test("terminal routes accept volume changes and reject bad integers", async (t) => {
  const s = await server();
  t.after(async () => {
    await s.http.close();
  });
  s.db.upsertTerminal({ id: "vocat-1", kind: "esp-vocat", companionId: "greg", ip: "", port: 0, lastSeen: 1, createdAt: "2026-01-01T00:00:00Z" });

  const list = (await (await fetch(`${s.base}/api/terminals`, { headers: ADMIN })).json()) as {
    terminals: Array<Record<string, unknown>>;
  };
  assert.equal(list.terminals[0]?.id, "vocat-1");
  const volume = await fetch(`${s.base}/api/terminals/vocat-1/volume`, {
    method: "POST",
    headers: ADMIN,
    body: JSON.stringify({ volume: 50 }),
  });
  assert.equal(volume.status, 200);
  assert.equal(s.db.getTerminalVolume("vocat-1"), 50);
  assert.equal(
    (await fetch(`${s.base}/api/terminals/vocat-1/volume`, { method: "POST", headers: ADMIN, body: JSON.stringify({ volume: 1.5 }) })).status,
    400,
  );
  assert.equal((await fetch(`${s.base}/api/terminals/vocat-1`, { method: "DELETE", headers: ADMIN })).status, 204);
  assert.equal(s.db.getTerminal("vocat-1"), undefined);
});

test("/health falls back to the edge while the router has no model state", async (t) => {
  const s = await server();
  t.after(async () => {
    await s.http.close();
  });
  s.edge.health = async () => ({ models: { kokoro: { state: "ready" }, parakeet: { state: "loading" } }, terminals: 1 });
  const health = (await (await fetch(`${s.base}/health`)).json()) as Record<string, unknown>;
  assert.equal(health.tts, true);
  assert.equal(health.stt, false);
  assert.equal(health.models_ready, false);
});

test("remote API rejects wrong methods, bad filters and out-of-scope terminals", async (t) => {
  const s = await server();
  t.after(async () => {
    await s.http.close();
  });
  const remote = { "x-lark-remote-token": s.token };
  assert.equal((await fetch(`${s.base}/remote`, { method: "POST", headers: remote })).status, 404);
  assert.equal((await fetch(`${s.base}/remote/api/say`, { headers: remote })).status, 404);
  assert.equal((await fetch(`${s.base}/remote/api/history?channel=email`, { headers: remote })).status, 400);
  assert.equal((await fetch(`${s.base}/remote/api/history?cursor=bm90LWEtY3Vyc29y`, { headers: remote })).status, 400);
  assert.equal(
    (
      await fetch(`${s.base}/remote/api/volume`, {
        method: "POST",
        headers: remote,
        body: JSON.stringify({ terminal: "someone-else", volume: 20 }),
      })
    ).status,
    403,
  );
  assert.equal(
    (
      await fetch(`${s.base}/remote/api/story/open`, {
        method: "POST",
        headers: remote,
        body: "{}",
      })
    ).status,
    400,
  );
  assert.equal((await fetch(`${s.base}/remote/api/portrait`, { headers: remote })).status, 404);
  assert.equal((await fetch(`${s.base}/remote/api/captures`, { headers: { "x-lark-remote-token": "nope" } })).status, 401);
});
