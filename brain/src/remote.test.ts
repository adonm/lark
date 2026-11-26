import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { Router } from "./router.ts";
import {
  CaptureStore,
  REMOTE_COOKIE,
  RemoteManager,
  parseRouteOutput,
  pruneCaptures,
  type TunnelRunner,
} from "./remote.ts";
import { startHttp, type RunningHttp } from "./http.ts";
import { FakeBrain, FakeEdge, settle } from "./testfakes.ts";
import type { EdgeClient } from "./edge_client.ts";

const WAV_HEADER = Buffer.alloc(44);
WAV_HEADER.write("RIFF", 0);
WAV_HEADER.writeUInt32LE(36 + 32000, 4);
WAV_HEADER.write("WAVEfmt ", 8);
WAV_HEADER.writeUInt32LE(16, 16);
WAV_HEADER.writeUInt16LE(1, 20);
WAV_HEADER.writeUInt16LE(1, 22);
WAV_HEADER.writeUInt32LE(16000, 24);
WAV_HEADER.writeUInt32LE(32000, 28);
WAV_HEADER.writeUInt16LE(2, 32);
WAV_HEADER.writeUInt16LE(16, 34);
WAV_HEADER.write("data", 36);
WAV_HEADER.writeUInt32LE(32000, 40);

function wavBytes(samples: number): Buffer {
  const data = Buffer.alloc(samples * 2);
  for (let i = 0; i < samples; i++) data.writeInt16LE(i % 2 === 0 ? 1000 : -1000, i * 2);
  const header = Buffer.from(WAV_HEADER);
  header.writeUInt32LE(36 + data.length, 4);
  header.writeUInt32LE(data.length, 40);
  return Buffer.concat([header, data]);
}

class FakeRunner {
  readonly calls: string[][] = [];
  fail = false;

  async run(args: readonly string[]): Promise<{ stdout: string; stderr: string; code: number }> {
    this.calls.push([...args]);
    if (this.fail) return { stdout: "", stderr: "offline", code: 1 };
    if (args[0] === "route" && args[1] === "add") {
      return { stdout: "route enabled: https://greg-tunnel.opentunnel.test\n", stderr: "", code: 0 };
    }
    return { stdout: "", stderr: "", code: 0 };
  }

  runner(): TunnelRunner {
    return (args) => this.run(args);
  }
}

function manager(_dir: string, db: LarkDb, runner: TunnelRunner): RemoteManager {
  return new RemoteManager({ db, localPort: 8765, runner, now: () => 1_760_000_000_000 });
}

test("tunnel route parsing accepts only public DNS names", () => {
  assert.deepEqual(parseRouteOutput("https://greg.opentunnel.test\n"), {
    routeName: "greg",
    url: "https://greg.opentunnel.test",
  });
  assert.equal(parseRouteOutput("https://localhost\n"), undefined);
  assert.equal(parseRouteOutput("not a url"), undefined);
});

test("token rotation persists the new link before retiring the old one", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-remote-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  const fake = new FakeRunner();
  const remote = manager(dir, db, fake.runner());

  const first = await remote.issue("greg", "sam@example.org", "Sam");
  assert.match(first, /^https:\/\/greg-tunnel\.opentunnel\.test\/remote\?token=/);
  const firstToken = new URL(first).searchParams.get("token") ?? "";
  assert.equal(remote.authorize(firstToken)?.companionId, "greg");
  assert.ok(db.getMeta("remote_route:greg")?.includes("greg-tunnel"));
  assert.ok(db.getMeta("remote_token:greg:sam@example.org")?.includes("hash"));

  const second = await remote.issue("greg", "sam@example.org", "Sam");
  const secondToken = new URL(second).searchParams.get("token") ?? "";
  assert.notEqual(firstToken, secondToken);
  assert.equal(remote.authorize(firstToken), undefined);
  assert.equal(remote.authorize(secondToken)?.contactName, "Sam");
  assert.equal(fake.calls.filter((call) => call[0] === "route" && call[1] === "add").length, 1);

  const tunnels = remote.listTunnels();
  assert.equal(tunnels.length, 1);
  assert.equal(tunnels[0]?.contacts, 1);
  assert.equal(tunnels[0]?.active, true);

  await remote.revokeContact("greg", "sam@example.org");
  assert.equal(remote.authorize(secondToken), undefined);
  assert.equal(remote.listTunnels().length, 0);
  assert.ok(fake.calls.some((call) => call[0] === "route" && call[1] === "remove"));
  assert.ok(fake.calls.some((call) => call[0] === "down"));
  assert.equal(db.getMeta("remote_route:greg"), undefined);
});

test("scoped links survive a brain restart", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-remote-restore-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  const fake = new FakeRunner();
  const first = manager(dir, db, fake.runner());
  const link = await first.issue("greg", "sam@example.org", "Sam");
  const token = new URL(link).searchParams.get("token") ?? "";
  const second = manager(dir, db, fake.runner());
  assert.equal(second.authorize(token)?.companionId, "greg");
  assert.equal(second.listTunnels()[0]?.contacts, 1);
  await second.close();
});

test("v1 bare-hash links keep working after upgrade", () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-remote-legacy-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  const raw = "legacy-link-token";
  db.setMeta("remote_token:greg:sam@example.org", createHash("sha256").update(raw).digest("hex"));
  const remote = manager(dir, db, new FakeRunner().runner());
  assert.equal(remote.authorize(raw)?.companionId, "greg");
});

test("pruneCaptures enforces retention and the byte budget", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-capture-test-"));
  const old = join(dir, "capture-greg-20200101-000000.wav");
  const oldText = join(dir, "capture-greg-20200101-000000.txt");
  const kept = join(dir, "capture-greg-20260101-000000.wav");
  writeFileSync(old, Buffer.alloc(1000));
  writeFileSync(oldText, "old\n");
  writeFileSync(kept, Buffer.alloc(1000));
  const newer = new Date();
  const older = new Date("2020-01-01T00:00:00Z");
  const { utimesSync } = await import("node:fs");
  utimesSync(kept, newer, newer);
  utimesSync(old, older, older);
  utimesSync(oldText, older, older);
  const removed = pruneCaptures(dir, 14 * 24 * 60 * 60 * 1000, 1 << 30);
  assert.equal(removed, 1004);
  assert.equal(readFileSync(kept).length, 1000);
  assert.throws(() => readFileSync(old));
  assert.throws(() => readFileSync(oldText));
  assert.equal(pruneCaptures(join(dir, "missing")), 0);
});

test("HTTP admin, remote page, SSE, history, stories, captures and revoke", async (t) => {
  const dir = mkdtempSync(join(tmpdir(), "lark-http-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", configured: true, voice: "af_heart" });
  db.saveStory({
    id: "lantern",
    title: "Lantern",
    twee: ":: StoryTitle\nLantern\n\n:: Start {\"end\": true}\nA small light.\n",
    author: "",
    updatedAt: 1,
    passages: 1,
  });
  const now = 1_760_000_000_000;
  const messages = [
    { id: "m1", role: "user", sender: "Someone", text: "hello room", channel: "in_person", ts: now },
    { id: "m2", role: "user", sender: "Sam", text: "hi from sam", channel: "deltachat", ts: now + 1000 },
    { id: "m3", role: "assistant", sender: "Greg", text: "replying to Sam", channel: "deltachat", ts: now + 2000 },
    { id: "m4", role: "user", sender: "Alex", text: "hi from alex", channel: "deltachat", ts: now + 3000 },
    { id: "m5", role: "assistant", sender: "Greg", text: "I am here", channel: "in_person", ts: now + 4000 },
  ] as const;
  for (const message of messages) {
    db.appendMessage({
      id: message.id,
      companionId: "greg",
      role: message.role,
      sender: message.sender,
      text: message.text,
      source: "test",
      channel: message.channel,
      cardTitle: "",
      tsMs: message.ts,
    });
  }
  const edge = new FakeEdge();
  edge.transcribeText = "capture words";
  const brain = new FakeBrain();
  const router = new Router({ db, edge, brain, dataDir: dir, now: () => now, log: () => undefined });
  edge.emit("terminal.connected", {
    terminal_id: "vocat-1",
    board: "esp-vocat",
    firmware: "0.21.0",
    width: 360,
    height: 360,
    shape: "round",
    ip: "127.0.0.1",
  });
  await settle();

  const fakeRunner = new FakeRunner();
  const remote = manager(dir, db, fakeRunner.runner());
  const captures = new CaptureStore({
    dir: join(dir, "debug"),
    edge: edge as unknown as EdgeClient,
    terminalForCompanion: () => "vocat-1",
    companionForTerminal: () => "greg",
    now: () => now,
  });
  const http: RunningHttp = await startHttp({
    host: "127.0.0.1",
    port: 0,
    adminToken: "admin-token",
    dataDir: dir,
    db,
    edge: edge as unknown as EdgeClient,
    router,
    remote,
    captures,
    libraryDir: join(dir, "library"),
    opencode: true,
  });
  t.after(async () => {
    await http.close();
  });
  const base = `http://127.0.0.1:${http.port}`;
  const token = (await remote.issueLocal("greg", "sam@example.org", "Sam")).split("token=")[1] ?? "";

  const health = await fetch(`${base}/health`);
  assert.equal(health.status, 200);
  const healthBody = (await health.json()) as Record<string, unknown>;
  assert.equal(healthBody.ok, true);

  assert.equal((await fetch(`${base}/api/companions`)).status, 401);
  assert.equal((await fetch(`${base}/api/companions`, { headers: { "x-lark-token": "wrong" } })).status, 401);
  const companions = await fetch(`${base}/api/companions`, { headers: { "x-lark-token": "admin-token" } });
  assert.equal(companions.status, 200);
  const companionBody = (await companions.json()) as { companions: Array<Record<string, unknown>> };
  assert.equal(companionBody.companions[0]?.id, "greg");
  assert.equal(companionBody.companions[0]?.online, true);

  const create = await fetch(`${base}/api/companions`, {
    method: "POST",
    headers: { "x-lark-token": "admin-token", "content-type": "application/json" },
    body: JSON.stringify({ name: "Moss", form: "a sleepy turtle" }),
  });
  assert.equal(create.status, 201);
  assert.equal(((await create.json()) as Record<string, unknown>).id, "moss");

  const page = await fetch(`${base}/remote?token=${encodeURIComponent(token)}`);
  assert.equal(page.status, 200);
  const html = await page.text();
  assert.match(html, /data-token="/);
  assert.match(html, /Greg/);
  assert.match(page.headers.get("content-security-policy") ?? "", /cdn\.jsdelivr\.net/);
  const cookie = page.headers.get("set-cookie") ?? "";
  assert.match(cookie, new RegExp(`${REMOTE_COOKIE}=`));
  assert.match(cookie, /HttpOnly/);
  assert.match(cookie, /Secure/);

  assert.equal((await fetch(`${base}/remote?token=wrong`)).status, 401);
  assert.equal((await fetch(`${base}/remote/page.css`)).status, 200);
  assert.equal((await fetch(`${base}/remote/app.js`)).status, 200);

  const sse = await fetch(`${base}/remote/events?token=${encodeURIComponent(token)}`);
  assert.equal(sse.status, 200);
  const reader = sse.body?.getReader();
  assert.ok(reader !== undefined);
  let buffered = "";
  while (!buffered.includes("event: state")) {
    const chunk = await reader.read();
    if (chunk.done) break;
    buffered += new TextDecoder().decode(chunk.value);
  }
  assert.match(buffered, /"name":"Greg"/);
  await reader.cancel();

  const all = (await (
    await fetch(`${base}/remote/api/history?channel=all&limit=20`, { headers: { "x-lark-remote-token": token } })
  ).json()) as { items: Array<Record<string, unknown>> };
  const texts = all.items.map((item) => item.text);
  assert.deepEqual(texts, ["hello room", "hi from sam", "I am here"]);
  assert.ok(!texts.includes("hi from alex"));
  assert.ok(!texts.includes("replying to Sam"));

  const onlyDc = (await (
    await fetch(`${base}/remote/api/history?channel=deltachat`, { headers: { "x-lark-remote-token": token } })
  ).json()) as { items: Array<Record<string, unknown>> };
  assert.deepEqual(onlyDc.items.map((item) => item.text), ["hi from sam"]);

  const search = (await (
    await fetch(`${base}/remote/api/history?q=room`, { headers: { "x-lark-remote-token": token } })
  ).json()) as { items: Array<Record<string, unknown>> };
  assert.deepEqual(search.items.map((item) => item.text), ["hello room"]);

  const say = await fetch(`${base}/remote/api/say`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: JSON.stringify({ text: "hello from the page" }),
  });
  assert.equal(say.status, 200);
  assert.match(edge.spoken.map((chunk) => chunk.text).join(" "), /hello from the page/);
  edge.emit("playback.done", { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  await settle();

  const beforeStory = edge.spoken.length;
  const story = await fetch(`${base}/remote/api/story/open`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: JSON.stringify({ story: "lantern" }),
  });
  assert.equal(story.status, 200);
  assert.equal(edge.lastScreen()?.kind, "reader");
  assert.equal(edge.spoken.length, beforeStory);

  const listen = await fetch(`${base}/remote/api/listen`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: JSON.stringify({ listening: true }),
  });
  assert.equal(listen.status, 200);
  assert.equal(edge.lastScreen()?.status, "listening");

  await fetch(`${base}/remote/api/listen`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: JSON.stringify({ listening: false }),
  });
  const manifest = await fetch(`${base}/remote/pwa/manifest.webmanifest`, { headers: { "x-lark-remote-token": token } });
  assert.equal(manifest.status, 200);
  const manifestBody = (await manifest.json()) as Record<string, unknown>;
  assert.equal(manifestBody.name, "Greg");
  assert.equal((await fetch(`${base}/remote/pwa/icon-192.png`, { headers: { "x-lark-remote-token": token } })).status, 200);

  const captureDir = join(dir, "debug");
  mkdirSync(captureDir, { recursive: true });
  const captureName = "capture-greg-20260101-000001";
  writeFileSync(join(captureDir, `${captureName}.wav`), wavBytes(16000));
  const list = (await (
    await fetch(`${base}/remote/api/captures`, { headers: { "x-lark-remote-token": token } })
  ).json()) as { captures: Array<Record<string, unknown>> };
  assert.equal(list.captures[0]?.name, captureName);
  assert.equal(list.captures[0]?.seconds, 1);

  const transcribe = await fetch(`${base}/remote/api/captures/${captureName}/transcribe`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: "{}",
  });
  assert.equal(transcribe.status, 200);
  assert.equal(((await transcribe.json()) as Record<string, unknown>).transcript, "capture words");
  assert.equal(readFileSync(join(captureDir, `${captureName}.txt`), "utf8"), "capture words\n");

  const wav = await fetch(`${base}/remote/api/captures/${captureName}.wav`, {
    headers: { "x-lark-remote-token": token, range: "bytes=0-99" },
  });
  assert.equal(wav.status, 206);
  assert.equal((await wav.arrayBuffer()).byteLength, 100);

  const started = await fetch(`${base}/remote/api/capture`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: JSON.stringify({ seconds: 5 }),
  });
  assert.equal(started.status, 200);
  const startedBody = (await started.json()) as Record<string, unknown>;
  edge.emitCaptureDone({
    terminal_id: "vocat-1",
    capture_id: String(startedBody.name),
    path: join(captureDir, `${String(startedBody.name)}.wav`),
    seconds: 5,
  });
  const status = captures.status("greg");
  assert.equal(status.state, "done");

  const deleted = await fetch(`${base}/remote/api/captures/${captureName}`, {
    method: "DELETE",
    headers: { "x-lark-remote-token": token },
  });
  assert.equal(deleted.status, 204);
  assert.throws(() => readFileSync(join(captureDir, `${captureName}.wav`)));

  const tunnels = (await (
    await fetch(`${base}/api/remote/tunnels`, { headers: { "x-lark-token": "admin-token" } })
  ).json()) as { tunnels: unknown[] };
  assert.equal(tunnels.tunnels.length, 0);

  const revoked = await fetch(`${base}/remote/api/revoke`, {
    method: "POST",
    headers: { "x-lark-remote-token": token, "content-type": "application/json" },
    body: "{}",
  });
  assert.equal(revoked.status, 200);
  assert.equal((await fetch(`${base}/remote/api/captures`, { headers: { "x-lark-remote-token": token } })).status, 401);
});

test("admin tunnel routes list and stop active companion routes", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-tunnel-http-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  const fakeRunner = new FakeRunner();
  const remote = manager(dir, db, fakeRunner.runner());
  await remote.issue("greg", "sam@example.org", "Sam");

  const edge = new FakeEdge();
  const brain = new FakeBrain();
  const router = new Router({ db, edge, brain, dataDir: dir, log: () => undefined });
  const captures = new CaptureStore({
    dir: join(dir, "debug"),
    edge: edge as unknown as EdgeClient,
    terminalForCompanion: () => undefined,
    companionForTerminal: () => undefined,
  });
  const http = await startHttp({
    host: "127.0.0.1",
    port: 0,
    adminToken: "t",
    dataDir: dir,
    db,
    edge: edge as unknown as EdgeClient,
    router,
    remote,
    captures,
    libraryDir: join(dir, "library"),
  });
  try {
    const base = `http://127.0.0.1:${http.port}`;
    const list = (await (await fetch(`${base}/api/remote/tunnels`, { headers: { "x-lark-token": "t" } })).json()) as {
      tunnels: Array<Record<string, unknown>>;
    };
    assert.equal(list.tunnels[0]?.companion_id, "greg");
    const stopped = await fetch(`${base}/api/remote/tunnels/greg`, { method: "DELETE", headers: { "x-lark-token": "t" } });
    assert.equal(stopped.status, 204);
    assert.equal(remote.listTunnels().length, 0);
  } finally {
    await http.close();
  }
});
