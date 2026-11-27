import assert from "node:assert/strict";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { createSocket, type Socket as UdpSocket } from "node:dgram";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import http from "node:http";
import { createConnection, createServer as createNetServer, type Socket } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { promisify } from "node:util";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { EdgeClient, type EdgeEventMap, type EdgeHit } from "./edge_client.ts";

const execFileAsync = promisify(execFile);
const repoRoot = resolve(import.meta.dirname, "../..");
const brainDir = resolve(import.meta.dirname, "..");
const EDGE_TOKEN = "e2e-edge-token";
const ADMIN_TOKEN = "e2e-admin-token";
const FIXED_STT = "set a timer for one minute";

// ---- terminal protocol ------------------------------------------------------

const MSG_HELLO = 0x01;
const MSG_PING = 0x0f;
const MSG_PONG = 0x10;
const MSG_WELCOME = 0x81;
const MSG_FRAME = 0x83;
const MSG_TTS_CTRL = 0x87;
const MSG_TTS_PCM = 0x88;
const MSG_TTS_ACK = 0x08;
const MSG_POINTER = 0x04;
const TTS_START = 1;
const TTS_END = 2;
const RTP_HEADER = 12;
const RTP_PCM = 640;

function pack(type: number, seq: number, payload: Buffer): Buffer {
  const header = Buffer.alloc(8);
  header[0] = 76;
  header[1] = type;
  header.writeUInt16LE(seq, 2);
  header.writeUInt32LE(payload.length, 4);
  return Buffer.concat([header, payload]);
}

function hello(terminalId: string): Buffer {
  const terminal = Buffer.from(terminalId);
  const companion = Buffer.from("greg");
  const board = Buffer.from("esp-vocat");
  const firmware = Buffer.from("0.21.0");
  const token = Buffer.from(EDGE_TOKEN);
  const payload = Buffer.alloc(21 + terminal.length + companion.length + board.length + firmware.length + token.length);
  payload[0] = 1;
  payload[1] = 0;
  payload.writeUInt32LE(0b1110001, 2); // battery, drag, pointer rev, bridge endpoint
  payload[6] = 1;
  payload[7] = 15;
  payload.writeUInt16LE(360, 8);
  payload.writeUInt16LE(360, 10);
  payload[12] = 0;
  payload[13] = 0;
  payload.writeUInt16LE(40000, 14);
  payload[16] = terminal.length;
  payload[17] = companion.length;
  payload[18] = board.length;
  payload[19] = firmware.length;
  payload[20] = token.length;
  let offset = 21;
  for (const part of [terminal, companion, board, firmware, token]) {
    part.copy(payload, offset);
    offset += part.length;
  }
  return payload;
}

interface Frame {
  type: number;
  seq: number;
  payload: Buffer;
}

interface Welcome {
  udpPort: number;
  ssrc: number;
}

class Hardware {
  readonly socket: Socket;
  connections = 1;
  #buffer = Buffer.alloc(0);
  readonly #queue: Frame[] = [];
  readonly #waiters: Array<{ resolve: (frame: Frame) => void; reject: (error: Error) => void; timer: ReturnType<typeof setTimeout>; type?: number }> = [];
  #closed = false;
  #seq = 0;
  #rtpSeq = 0;

  constructor(socket: Socket) {
    this.socket = socket;
    socket.on("data", (chunk: Buffer) => this.#feed(chunk));
    socket.on("close", () => {
      this.#closed = true;
      for (const waiter of this.#waiters.splice(0)) {
        clearTimeout(waiter.timer);
        waiter.reject(new Error("hardware socket closed"));
      }
    });
    socket.on("error", () => undefined);
  }

  static async connect(address: string, terminalId: string): Promise<{ hardware: Hardware; welcome: Welcome }> {
    const socket = await new Promise<Socket>((resolveConnect, rejectConnect) => {
      const value = createConnection({ host: address.split(":")[0], port: Number(address.split(":")[1]) });
      value.once("connect", () => resolveConnect(value));
      value.once("error", rejectConnect);
    });
    const hardware = new Hardware(socket);
    hardware.send(MSG_HELLO, hello(terminalId));
    const frame = await hardware.next(MSG_WELCOME, 5000);
    const payload = frame.payload;
    if (payload.length < 38) throw new Error("short welcome");
    return { hardware, welcome: { udpPort: payload.readUInt16LE(2), ssrc: payload.readUInt32LE(4) } };
  }

  #feed(chunk: Buffer): void {
    this.#buffer = Buffer.concat([this.#buffer, chunk]);
    for (;;) {
      if (this.#buffer.length < 8) return;
      if (this.#buffer[0] !== 76) throw new Error("bad magic");
      const type = this.#buffer[1] ?? 0;
      const seq = this.#buffer.readUInt16LE(2);
      const length = this.#buffer.readUInt32LE(4);
      if (this.#buffer.length < 8 + length) return;
      const payload = this.#buffer.subarray(8, 8 + length);
      this.#buffer = this.#buffer.subarray(8 + length);
      this.#deliver({ type, seq, payload: Buffer.from(payload) });
    }
  }

  #deliver(frame: Frame): void {
    const index = this.#waiters.findIndex((waiter) => waiter.type === undefined || waiter.type === frame.type);
    if (index >= 0) {
      const [waiter] = this.#waiters.splice(index, 1);
      if (waiter !== undefined) {
        clearTimeout(waiter.timer);
        waiter.resolve(frame);
      }
      return;
    }
    this.#queue.push(frame);
  }

  send(type: number, payload: Buffer): void {
    this.socket.write(pack(type, this.#seq++, payload));
  }

  next(type?: number, timeoutMs = 5000): Promise<Frame> {
    for (let index = 0; index < this.#queue.length; index++) {
      const frame = this.#queue[index];
      if (frame === undefined) continue;
      if (type === undefined || frame.type === type) {
        this.#queue.splice(index, 1);
        return Promise.resolve(frame);
      }
    }
    if (this.#closed) return Promise.reject(new Error("hardware socket closed"));
    return new Promise<Frame>((resolveFrame, rejectFrame) => {
      const timer = setTimeout(() => {
        const at = this.#waiters.findIndex((entry) => entry.timer === timer);
        if (at >= 0) this.#waiters.splice(at, 1);
        rejectFrame(new Error(`timed out waiting for message 0x${(type ?? 0).toString(16)}`));
      }, timeoutMs);
      this.#waiters.push({ resolve: resolveFrame, reject: rejectFrame, timer, ...(type === undefined ? {} : { type }) });
    });
  }

  async nextReady(timeoutMs = 5000): Promise<Frame> {
    for (;;) {
      const frame = await this.next(undefined, timeoutMs);
      if (frame.type === MSG_PING) {
        this.send(MSG_PONG, frame.payload.subarray(0, 4));
        continue;
      }
      return frame;
    }
  }

  tap(x: number, y: number): void {
    const payload = Buffer.alloc(8);
    payload[0] = 0;
    payload.writeUInt16LE(Math.round(x), 2);
    payload.writeUInt16LE(Math.round(y), 4);
    payload.writeUInt16LE(0, 6);
    this.send(MSG_POINTER, payload);
  }

  ping(payload = Buffer.from([1, 2, 3, 4])): Promise<Frame> {
    this.send(MSG_PING, payload);
    return this.next(MSG_PONG, 3000);
  }

  async rtp(udp: UdpSocket, target: { address: string; port: number }, ssrc: number, pcm: Buffer): Promise<void> {
    for (let offset = 0; offset < pcm.length; offset += RTP_PCM) {
      const chunk = pcm.subarray(offset, offset + RTP_PCM);
      const packet = Buffer.alloc(RTP_HEADER + chunk.length);
      packet[0] = 128;
      packet[1] = 96;
      this.#rtpSeq = (this.#rtpSeq + 1) & 0xffff;
      packet.writeUInt16BE(this.#rtpSeq, 2);
      packet.writeUInt32BE(this.#rtpSeq * 320, 4);
      packet.writeUInt32BE(ssrc, 8);
      chunk.copy(packet, RTP_HEADER);
      await new Promise<void>((resolveSend, rejectSend) => udp.send(packet, target.port, target.address, (error) => (error ? rejectSend(error) : resolveSend())));
      // Pace like a real microphone: 640 bytes is 20 ms of 16 kHz mono.
      await sleep(20);
    }
  }

  async readTts(timeoutMs = 20_000): Promise<{ audioId: number; bytes: number; textStreams: number }> {
    const deadline = Date.now() + timeoutMs;
    let audioId = 0;
    let bytes = 0;
    let starts = 0;
    for (;;) {
      const frame = await this.nextReady(Math.max(1, deadline - Date.now()));
      if (frame.type === MSG_TTS_CTRL) {
        const op = frame.payload[0] ?? 0;
        const id = Number(frame.payload.readBigUInt64LE(1));
        if (op === TTS_START) {
          audioId = id;
          starts++;
        } else if (op === TTS_END && id === audioId) {
          this.send(MSG_TTS_ACK, Buffer.concat([Buffer.from([0]), (() => {
            const out = Buffer.alloc(8);
            out.writeBigUInt64LE(BigInt(audioId));
            return out;
          })()]));
          return { audioId, bytes, textStreams: starts };
        }
      } else if (frame.type === MSG_TTS_PCM) {
        bytes += frame.payload.length;
      }
    }
  }

  async expectNoTts(windowMs: number): Promise<void> {
    const deadline = Date.now() + windowMs;
    for (;;) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) return;
      let frame: Frame;
      try {
        frame = await this.nextReady(remaining);
      } catch {
        return;
      }
      assert.notEqual(frame.type, MSG_TTS_CTRL, "story reader must not synthesize speech");
    }
  }

  async drainTts(windowMs: number): Promise<void> {
    const deadline = Date.now() + windowMs;
    let audioId = 0;
    while (Date.now() < deadline) {
      let frame: Frame;
      try {
        frame = await this.nextReady(deadline - Date.now());
      } catch {
        return;
      }
      if (frame.type === MSG_TTS_CTRL) {
        const op = frame.payload[0] ?? 0;
        const id = Number(frame.payload.readBigUInt64LE(1));
        if (op === TTS_START) audioId = id;
        if (op === TTS_END && id === audioId) {
          const ack = Buffer.alloc(9);
          ack[0] = 0;
          ack.writeBigUInt64LE(BigInt(audioId), 1);
          this.send(MSG_TTS_ACK, ack);
        }
      }
    }
  }

  close(): void {
    this.socket.destroy();
  }
}

function tone(samples: number): Buffer {
  const bytes = Buffer.alloc(samples * 2);
  for (let i = 0; i < samples; i++) bytes.writeInt16LE(i % 2 === 0 ? 4000 : -4000, i * 2);
  return bytes;
}

function silence(samples: number): Buffer {
  return Buffer.alloc(samples * 2);
}

// ---- helpers ----------------------------------------------------------------

async function freePort(): Promise<number> {
  const server = createNetServer();
  await new Promise<void>((resolveListen) => server.listen(0, "127.0.0.1", resolveListen));
  const address = server.address();
  const port = address !== null && typeof address === "object" ? address.port : 0;
  await new Promise<void>((resolveClose) => server.close(() => resolveClose()));
  return port;
}

interface Managed {
  child: ChildProcess;
  logs: string[];
}

function capture(child: ChildProcess, label: string): Managed {
  const logs: string[] = [];
  child.stdout?.on("data", (chunk: Buffer) => logs.push(`${label}: ${chunk.toString()}`));
  child.stderr?.on("data", (chunk: Buffer) => logs.push(`${label}: ${chunk.toString()}`));
  child.on("exit", (code, signal) => logs.push(`${label}: exited ${String(code ?? signal)}`));
  return { child, logs };
}

async function waitExit(child: ChildProcess, timeoutMs: number): Promise<boolean> {
  if (child.exitCode !== null || child.signalCode !== null) return true;
  return await new Promise<boolean>((resolveExit) => {
    const timer = setTimeout(() => resolveExit(false), timeoutMs);
    child.once("exit", () => {
      clearTimeout(timer);
      resolveExit(true);
    });
  });
}

async function waitUntil(check: () => boolean | Promise<boolean>, timeoutMs: number, label: string): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await check()) return;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${label}`);
    await sleep(50);
  }
}

async function waitHttp(url: string, timeoutMs = 10_000): Promise<void> {
  await waitUntil(async () => {
    try {
      const response = await fetch(url);
      return response.status < 500;
    } catch {
      return false;
    }
  }, timeoutMs, url);
}

// ---- mock OpenAI-compatible model ------------------------------------------

interface WireMessage {
  role: string;
  content?: string | Array<{ type: string; text?: string }>;
  tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }>;
}

interface WireRequest {
  model?: string;
  messages?: WireMessage[];
  tools?: Array<{ function?: { name?: string } }>;
}

function textOf(message: WireMessage | undefined): string {
  if (message === undefined) return "";
  if (typeof message.content === "string") return message.content;
  if (Array.isArray(message.content)) return message.content.map((part) => (part.type === "text" ? part.text ?? "" : "")).join("");
  return "";
}

function lastUserIndex(request: WireRequest): number {
  const messages = request.messages ?? [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message?.role !== "user") continue;
    if (textOf(message).startsWith("<system-update>")) continue;
    return i;
  }
  return -1;
}

function hasToolResult(request: WireRequest): boolean {
  const messages = request.messages ?? [];
  return messages.slice(lastUserIndex(request) + 1).some((message) => message.role === "tool");
}

class MockModel {
  readonly requests: WireRequest[] = [];
  baseURL = "";
  readonly #server: http.Server;

  constructor() {
    this.#server = http.createServer((request, response) => {
      const chunks: Buffer[] = [];
      request.on("data", (chunk: Buffer) => chunks.push(chunk));
      request.on("end", () => {
        let body: WireRequest = {};
        try {
          body = JSON.parse(Buffer.concat(chunks).toString("utf8")) as WireRequest;
        } catch {
          body = {};
        }
        this.requests.push(body);
        response.writeHead(200, { "content-type": "text/event-stream", connection: "keep-alive" });
        const base = { id: "chatcmpl-e2e", object: "chat.completion.chunk", created: 1, model: body.model ?? "mock" };
        const send = (delta: unknown, finish: string | null = null): void => {
          response.write(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`);
        };
        const finish = (): void => {
          send({}, "stop");
          response.write("data: [DONE]\n\n");
          response.end();
        };
        if ((body.tools ?? []).length === 0) {
          send({ content: "Lantern" });
          finish();
          return;
        }
        const prompt = textOf((body.messages ?? [])[lastUserIndex(body)]);
        const toolResult = hasToolResult(body);
        if (prompt.includes(FIXED_STT) && !toolResult) {
          send({
            role: "assistant",
            tool_calls: [
              {
                index: 0,
                id: `call_e2e_${this.requests.length}`,
                type: "function",
                function: {
                  name: "lark_patch",
                  arguments: JSON.stringify({
                    companion_id: "greg",
                    timer: { in_seconds: 60, label: "tea", say: "Tea is ready.", chime: "tap" },
                  }),
                },
              },
            ],
          });
          send({}, "tool_calls");
          response.write("data: [DONE]\n\n");
          response.end();
          return;
        }
        if (prompt.includes(FIXED_STT) && toolResult) {
          send({ content: "I set a timer for one minute." });
          finish();
          return;
        }
        if (prompt.includes("via Delta Chat")) {
          send({ content: "Hello Sam. Nice to hear from you." });
          finish();
          return;
        }
        send({ content: "Okay." });
        finish();
      });
    });
  }

  async start(): Promise<void> {
    await new Promise<void>((resolveListen) => this.#server.listen(0, "127.0.0.1", resolveListen));
    const address = this.#server.address();
    if (address === null || typeof address === "string") throw new Error("mock model has no port");
    this.baseURL = `http://127.0.0.1:${address.port}/v1`;
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolveClose) => this.#server.close(() => resolveClose()));
  }

  toolNames(): string[] {
    const names = new Set<string>();
    for (const request of this.requests) {
      for (const tool of request.tools ?? []) if (tool.function?.name !== undefined) names.add(tool.function.name);
    }
    return [...names];
  }
}

// ---- test -------------------------------------------------------------------

test("real Go edge + Node brain + stub Delta Chat end to end", { timeout: 180_000 }, async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-e2e-"));
  const dataDir = join(dir, "data");
  const sock = join(dataDir, "edge.sock");
  mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  mkdirSync(join(dataDir, "library"), { recursive: true });
  const dbPath = join(dataDir, "lark.db");
  const db = new LarkDb(dbPath);
  db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", personality: "dry", voice: "af_heart", configured: true });
  db.upsertTerminal({ id: "vocat-e2e", kind: "esp-vocat", companionId: "greg", ip: "127.0.0.1", port: 0, lastSeen: Date.now(), createdAt: new Date().toISOString() });
  db.saveStory({
    id: "lantern",
    title: "Lantern",
    twee: ":: StoryTitle\nLantern\n\n:: Start {\"end\": true}\nA small lantern waits on the sill.\n",
    author: "",
    updatedAt: 1,
    passages: 1,
  });
  db.close();

  const edgeBin = join(dir, "lark-edge");
  await execFileAsync("go", ["build", "-o", edgeBin, "./cmd/lark-edge"], { cwd: join(repoRoot, "lark") });
  const mock = new MockModel();
  await mock.start();
  const httpPort = await freePort();
  const dcPort = await freePort();

  const stubPath = join(dir, "stub-dc.mjs");
  copyFileSync(join(brainDir, "test/stub-dc.mjs"), stubPath);
  chmodSync(stubPath, 0o755);
  const tunnelPath = join(dir, "opentunnel");
  writeFileSync(tunnelPath, `#!/bin/sh\necho "https://greg-e2e.opentunnel.test"\n`);
  chmodSync(tunnelPath, 0o755);
  mkdirSync(join(dataDir, "ota"), { recursive: true });
  writeFileSync(join(dataDir, "ota", "version.json"), JSON.stringify({ version: "9.9.9" }));

  const managed: Managed[] = [];
  const brainEnv: NodeJS.ProcessEnv = {
    ...process.env,
    LARK_DATA_DIR: dataDir,
    LARK_DB_PATH: dbPath,
    LARK_EDGE_SOCK: sock,
    LARK_HOST: "127.0.0.1",
    LARK_PORT: String(httpPort),
    LARK_TOKEN: ADMIN_TOKEN,
    LARK_DIRECTORY: repoRoot,
    LARK_AGENT_FILE: join(repoRoot, "agents/lark.md"),
    LARK_OPENCODE_DIR: join(dataDir, "opencode"),
    LARK_LIBRARY_DIR: join(dataDir, "library"),
    LARK_LLM_BASE_URL: mock.baseURL,
    LARK_LLM_MODEL: "mock",
    LARK_DC_RPC_BIN: stubPath,
    LARK_DC_STUB_PORT: String(dcPort),
    LARK_CHATMAIL_RELAY: "nine.testrun.org",
    LARK_OPENTUNNEL_BIN: tunnelPath,
    LARK_EDGE_FAKE_STT: FIXED_STT,
    LARK_TOKEN_FILE: "",
  };
  const spawnBrain = (label: string): Managed => {
    const child = spawn(process.execPath, ["src/main.ts"], { cwd: brainDir, env: brainEnv, stdio: ["ignore", "pipe", "pipe"] });
    const value = capture(child, label);
    managed.push(value);
    return value;
  };

  let observer: EdgeClient | undefined;
  let hardware: Hardware | undefined;
  let udp: UdpSocket | undefined;
  try {
    const edge = spawn(edgeBin, ["-addr", "127.0.0.1:0", "-sock", sock, "-fake-models"], {
      env: { ...process.env, LARK_TOKEN: EDGE_TOKEN, LARK_EDGE_FAKE_STT: FIXED_STT },
      stdio: ["ignore", "pipe", "pipe"],
    });
    const edgeManaged = capture(edge, "edge");
    managed.push(edgeManaged);
    await waitUntil(() => edgeManaged.logs.some((line) => line.includes("terminal=")), 10_000, "edge listen line");
    const line = edgeManaged.logs.find((value) => value.includes("terminal="));
    assert.ok(line !== undefined, edgeManaged.logs.join(""));
    const terminalMatch = /terminal=([0-9.]+:\d+)/.exec(line);
    const udpMatch = /udp=([0-9.]+:\d+)/.exec(line);
    assert.ok(terminalMatch?.[1] !== undefined && udpMatch?.[1] !== undefined, line);
    const terminalAddress = terminalMatch[1];
    const udpAddress = udpMatch[1];

    const connected = await Hardware.connect(terminalAddress, "vocat-e2e");
    hardware = connected.hardware;
    const device = connected.hardware;
    const welcome = connected.welcome;
    udp = createSocket("udp4");
    const udpSocket = udp;
    const target = { address: udpAddress.split(":")[0] ?? "127.0.0.1", port: Number(udpAddress.split(":")[1]) };
    assert.ok(welcome.ssrc > 0);

    observer = new EdgeClient({ socketPath: sock, minBackoffMs: 50, maxBackoffMs: 300 });
    observer.start();
    await observer.waitReady(5000);
    const speculate: Array<EdgeEventMap["turn.speculate"]> = [];
    const commits: Array<EdgeEventMap["turn.commit"]> = [];
    const playback: Array<EdgeEventMap["playback.done"]> = [];

    observer.on("turn.speculate", (event) => speculate.push(event));
    observer.on("turn.commit", (event) => commits.push(event));
    observer.on("playback.done", (event) => playback.push(event));

    const waitHits = async (id: string): Promise<EdgeHit> => {
      const deadline = Date.now() + 15_000;
      for (;;) {
        try {
          const reply = await observer?.hits("vocat-e2e");
          const hit = reply?.hits.find((entry) => entry.ID === id);
          if (hit !== undefined) return hit;
        } catch {
          // Renderer still working.
        }
        if (Date.now() > deadline) throw new Error(`timed out waiting for hit ${id}`);
        await sleep(60);
      }
    };
    const tapHit = async (id: string): Promise<void> => {
      const hit = await waitHits(id);
      device.tap((hit.X1 + hit.X2) / 2, (hit.Y1 + hit.Y2) / 2);
    };
    const waitHome = async (): Promise<void> => {
      const deadline = Date.now() + 15_000;
      for (;;) {
        const frame = await device.nextReady(Math.max(1, deadline - Date.now()));
        if (frame.type === MSG_FRAME) return;
      }
    };

    let brain = spawnBrain("brain-1");
    // Wait for the child to report ready before spending the frame budget.
    // Brain.create and the DC/library setup run before the edge IPC client
    // attaches, and that cold start can exceed a frame timeout under load.
    await waitHttp(`http://127.0.0.1:${httpPort}/health`, 20_000);
    await waitHome();

    // Menu → Stories → open the story silently.
    await tapHit("menu");
    await waitHome();
    await tapHit("menu-stories");
    await waitHome();
    await tapHit("lantern");
    await waitHome();
    const readerHits = await observer.hits("vocat-e2e");
    assert.equal(readerHits.screen, "reader");
    await device.expectNoTts(700);

    // Back to home: reader → stories list → menu → home.
    for (const expected of ["list", "menu", "home"]) {
      await tapHit("menu-back");
      await waitHome();
      const reply: Awaited<ReturnType<EdgeClient["hits"]>> = await observer.hits("vocat-e2e");
      assert.equal(reply.screen, expected);
    }

    // Tap Listen, speak, commit, native timer patch, streamed speech, TTS ack.
    await tapHit("listen");
    await waitHome();
    await sleep(250);
    await device.rtp(udpSocket, target, welcome.ssrc, tone(16_000));
    await sleep(300);
    await device.rtp(udpSocket, target, welcome.ssrc, silence(16_000 * 18 / 100));
    await sleep(300);
    await device.rtp(udpSocket, target, welcome.ssrc, silence(16_000 * 30 / 100));
    await waitUntil(() => commits.length > 0, 20_000, "turn.commit");
    assert.equal(commits[0]?.transcript, FIXED_STT);
    assert.ok(speculate.length >= 1, "edge should speculate before commit");
    const tts = await device.readTts(30_000);
    assert.ok(tts.bytes > 0, "edge synthesized audio for the streamed reply");
    await waitUntil(() => playback.length > 0, 10_000, "playback.done");
    const checkDb = new LarkDb(dbPath);
    try {
      assert.equal(checkDb.countTimers("greg"), 1, "native lark_patch created the timer");
    } finally {
      checkDb.close();
    }
    assert.ok(mock.toolNames().includes("lark_patch"));

    // Restart the brain while the edge and hardware keep their TCP session.
    assert.equal(device.connections, 1);
    await device.ping();
    brain.child.kill("SIGTERM");
    assert.equal(await waitExit(brain.child, 15_000), true, brain.logs.join(""));
    await device.ping();
    assert.equal(device.connections, 1);

    brainEnv.LARK_DC_STUB_PORT = String(await freePort());
    brain = spawnBrain("brain-2");
    await waitHttp(`http://127.0.0.1:${httpPort}/health`, 20_000);
    await waitHome();
    await device.ping();
    assert.equal(device.connections, 1);

    // The restarted brain handles a fresh turn without reconnecting hardware.
    await tapHit("listen");
    await waitHome();
    await sleep(200);
    await device.rtp(udpSocket, target, welcome.ssrc, tone(16_000));
    await sleep(300);
    await device.rtp(udpSocket, target, welcome.ssrc, silence(16_000 * 18 / 100));
    await sleep(300);
    await device.rtp(udpSocket, target, welcome.ssrc, silence(16_000 * 30 / 100));
    await waitUntil(() => commits.length >= 2, 15_000, "second turn.commit");
    await device.readTts(30_000);
    await waitUntil(() => playback.length >= 2, 10_000, "second playback.done");
    assert.equal(device.connections, 1);

    // Delta Chat text-only round trip. The restarted stub loads the persisted
    // Chatmail account so the companion keeps its address.
    const dcUrl = `http://127.0.0.1:${brainEnv.LARK_DC_STUB_PORT ? Number(brainEnv.LARK_DC_STUB_PORT) : dcPort}`;
    await waitHttp(`${dcUrl}/state`, 20_000);
    await waitUntil(async () => {
      const response = await fetch(`${dcUrl}/state`);
      const body = (await response.json()) as { accounts: Array<{ address: string }> };
      return body.accounts.some((account) => account.address.endsWith("@nine.testrun.org"));
    }, 20_000, "restored Delta Chat account");
    await fetch(`${dcUrl}/inject`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ sender: "Sam", address: "sam@example.org", text: "hello Greg" }),
    });
    await waitUntil(async () => {
      const response = await fetch(`${dcUrl}/sent`);
      const body = (await response.json()) as { sent: Array<{ data?: { text?: string } }> };
      return body.sent.some((entry) => entry.data?.text === "Hello Sam. Nice to hear from you.");
    }, 20_000, "Delta Chat reply");
    await device.drainTts(4000);

    await fetch(`${dcUrl}/inject`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ sender: "Alex", address: "alex@example.org", text: "hi from alex" }),
    });
    await sleep(500);
    await fetch(`${dcUrl}/inject`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ sender: "Sam", address: "sam@example.org", text: "/remote" }),
    });
    let remoteLink = "";
    await waitUntil(async () => {
      const response = await fetch(`${dcUrl}/sent`);
      const body = (await response.json()) as { sent: Array<{ data?: { text?: string } }> };
      const found = body.sent.map((entry) => entry.data?.text ?? "").find((text) => text.includes("/remote?token="));
      if (found !== undefined) remoteLink = found;
      return found !== undefined;
    }, 20_000, "/remote link");
    const remoteToken = new URL(remoteLink).searchParams.get("token") ?? "";
    assert.ok(remoteToken.length > 10);

    const base = `http://127.0.0.1:${httpPort}`;
    const page = await fetch(`${base}/remote?token=${encodeURIComponent(remoteToken)}`);
    assert.equal(page.status, 200);
    assert.match(await page.text(), /Greg/);
    assert.match(page.headers.get("content-security-policy") ?? "", /cdn\.jsdelivr\.net/);
    assert.match(page.headers.get("set-cookie") ?? "", /HttpOnly/);

    const events = await fetch(`${base}/remote/events?token=${encodeURIComponent(remoteToken)}`);
    assert.equal(events.status, 200);
    const reader = events.body?.getReader();
    assert.ok(reader !== undefined);
    let sse = "";
    while (!sse.includes("event: state")) {
      const chunk = await reader.read();
      if (chunk.done) break;
      sse += new TextDecoder().decode(chunk.value);
    }
    assert.match(sse, /"name":"Greg"/);
    await reader.cancel();

    const history = (await (
      await fetch(`${base}/remote/api/history?channel=all&limit=50`, { headers: { "x-lark-remote-token": remoteToken } })
    ).json()) as { items: Array<{ text: string; channel: string }> };
    const texts = history.items.map((item) => item.text);
    assert.ok(texts.includes("hello Greg"), "contact's own Delta Chat message is visible");
    assert.ok(texts.includes(FIXED_STT), "in-person voice turns are visible");
    assert.ok(!texts.some((text) => text.includes("hi from alex")), "another contact's message is hidden");
    // Both stub replies read the same, so the count proves only Sam's row is
    // scoped into this token's history.
    assert.equal(
      texts.filter((text) => text.includes("Hello Sam")).length,
      1,
      "the companion's reply to this contact is visible and Alex's is hidden",
    );

    const tunnels = (await (
      await fetch(`${base}/api/remote/tunnels`, { headers: { "x-lark-token": ADMIN_TOKEN } })
    ).json()) as { tunnels: Array<Record<string, unknown>> };
    assert.equal(tunnels.tunnels[0]?.companion_id, "greg");

    const status = await execFileAsync(process.execPath, ["src/cli.ts", "status"], {
      cwd: brainDir,
      env: { ...process.env, LARK_URL: base, LARK_TOKEN: ADMIN_TOKEN },
    });
    assert.match(status.stdout, /Server: ok=true/);
    assert.match(status.stdout, /greg\tGreg/);
    const doctor = await execFileAsync(process.execPath, ["src/cli.ts", "doctor"], {
      cwd: brainDir,
      env: { ...process.env, LARK_URL: base, LARK_TOKEN: ADMIN_TOKEN, LARK_EDGE_SOCK: sock },
    });
    assert.match(doctor.stdout, /Doctor: all checks passed/);

    // Graceful shutdown leaves the hardware session untouched during its own
    // restart; the edge itself is only stopped at cleanup.
    brain.child.kill("SIGTERM");
    assert.equal(await waitExit(brain.child, 15_000), true, brain.logs.join(""));
  } finally {
    for (const value of managed) {
      if (value.child.exitCode === null && value.child.signalCode === null) value.child.kill("SIGTERM");
    }
    await sleep(100);
    for (const value of managed) {
      if (value.child.exitCode === null && value.child.signalCode === null) value.child.kill("SIGKILL");
    }
    await observer?.close().catch(() => undefined);
    hardware?.close();
    udp?.close();
    await mock.stop().catch(() => undefined);
  }
});
