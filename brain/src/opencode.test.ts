import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import http from "node:http";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { after, before, test } from "node:test";
import { LarkDb } from "./db.ts";
import { Library } from "./library.ts";
import {
  Brain,
  DEFAULT_MODELS,
  SpeculationAbortedError,
  parseAgentMarkdown,
  parseModelSpec,
  readOpenCodeCredentials,
  stripState,
} from "./opencode.ts";
import { TimerScheduler } from "./timers.ts";

interface WireMessage {
  role: string;
  content?: string | Array<{ type: string; text?: string }>;
  tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }>;
  tool_call_id?: string;
}

interface WireRequest {
  model?: string;
  messages?: WireMessage[];
  tools?: Array<{ function?: { name?: string } }>;
}

function textOf(message: WireMessage | undefined): string {
  if (message === undefined) return "";
  if (typeof message.content === "string") return message.content;
  if (Array.isArray(message.content)) {
    return message.content.map((part) => (part.type === "text" ? part.text ?? "" : "")).join("");
  }
  return "";
}

function lastPromptIndex(request: WireRequest): number {
  const messages = request.messages ?? [];
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i];
    if (message?.role !== "user") continue;
    // The SDK injects instruction-entry updates as synthetic user messages.
    if (textOf(message).startsWith("<system-update>")) continue;
    return i;
  }
  return -1;
}

function lastUser(request: WireRequest): string {
  const index = lastPromptIndex(request);
  return index < 0 ? "" : textOf((request.messages ?? [])[index]);
}

function afterLastUserTool(request: WireRequest): boolean {
  const messages = request.messages ?? [];
  return messages.slice(lastPromptIndex(request) + 1).some((message) => message.role === "tool");
}

const TITLE_TWEE = ":: StoryTitle\nLantern\n\n:: Start {\"end\": true}\nA small light.\n";

class MockModel {
  readonly requests: WireRequest[] = [];
  #server: http.Server;
  baseURL = "";

  constructor() {
    this.#server = http.createServer((request, response) => {
      let body = "";
      request.on("data", (chunk) => {
        body += chunk;
      });
      request.on("end", () => {
        let parsed: WireRequest = {};
        try {
          parsed = JSON.parse(body) as WireRequest;
        } catch {
          parsed = {};
        }
        this.requests.push(parsed);
        response.on("error", () => undefined);
        response.writeHead(200, { "content-type": "text/event-stream" });
        const send = (chunk: unknown): void => {
          if (!response.destroyed) response.write(`data: ${JSON.stringify(chunk)}\n\n`);
        };
        const base = { id: "chatcmpl-mock", object: "chat.completion.chunk", created: 1, model: parsed.model ?? "mock" };
        const finish = (): void => {
          send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
          response.write("data: [DONE]\n\n");
          response.end();
        };
        const tools = parsed.tools ?? [];
        if (tools.length === 0) {
          send({ ...base, choices: [{ index: 0, delta: { content: "Title" }, finish_reason: null }] });
          finish();
          return;
        }
        if (afterLastUserTool(parsed)) {
          const content = lastUser(parsed).includes("TOOLSTEP") ? "second" : "Tool finished.";
          send({ ...base, choices: [{ index: 0, delta: { content }, finish_reason: null }] });
          finish();
          return;
        }
        const prompt = lastUser(parsed);
        if (prompt.includes("STALL") && parsed.model === "m2") {
          send({ ...base, choices: [{ index: 0, delta: { content: "recovered" }, finish_reason: null }] });
          finish();
          return;
        }
        const call = (name: string, args: unknown): void => {
          send({
            ...base,
            choices: [
              {
                index: 0,
                delta: {
                  role: "assistant",
                  tool_calls: [
                    {
                      index: 0,
                      id: `call_${this.requests.length}`,
                      type: "function",
                      function: { name, arguments: JSON.stringify(args) },
                    },
                  ],
                },
                finish_reason: null,
              },
            ],
          });
          send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: "tool_calls" }] });
          response.write("data: [DONE]\n\n");
          response.end();
        };
        if (prompt.includes("TOOLSTEP")) {
          send({ ...base, choices: [{ index: 0, delta: { content: "first" }, finish_reason: null }] });
          call("lark_state", { companion_id: "cake", include: ["me"] });
          return;
        }
        if (prompt.includes("TOOLSTATE")) {
          call("lark_state", { companion_id: "cake", include: ["me", "messages"] });
          return;
        }
        if (prompt.includes("TOOLPATCH")) {
          call("lark_patch", { companion: { listening: true } });
          return;
        }
        if (prompt.includes("BADPATCH")) {
          call("lark_patch", { companion: { name: "ShouldNotStick", bogus: 1 } });
          return;
        }
        if (prompt.includes("TOOLSTORY")) {
          call("lark_story", { op: "create", twee: TITLE_TWEE });
          return;
        }
        if (prompt.includes("TOOLLIBRARY")) {
          call("lark_library", { op: "search", query: "otters", source: "simple" });
          return;
        }
        if (prompt.includes("TOOLAUDIO")) {
          call("lark_audio", { op: "metronome", bpm: 100, beats: 4, duration_s: 10 });
          return;
        }
        if (prompt.includes("BADAUDIO")) {
          call("lark_audio", { op: "tune", notes: [{ note: "C4", beats: 1 }], bogus: true });
          return;
        }
        if (prompt.includes("GATEPATCH")) {
          call("lark_patch", { companion: { name: "Patched" } });
          return;
        }
        if (prompt.includes("STALL") && parsed.model !== "m2") {
          // Never produce a first token; the brain's stall watchdog must move
          // the turn to the fallback model.
          setTimeout(() => {
            if (response.destroyed) return;
            send({ ...base, choices: [{ index: 0, delta: { content: "too late" }, finish_reason: null }] });
            send({ ...base, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
            response.write("data: [DONE]\n\n");
            response.end();
          }, 3000);
          return;
        }
        if (prompt.includes("GATEABORT")) {
          call("lark_patch", { companion: { name: "Aborted" } });
          return;
        }
        for (const word of ["Hello", " from", " the", " brain"]) {
          send({ ...base, choices: [{ index: 0, delta: { content: word }, finish_reason: null }] });
        }
        finish();
      });
    });
  }

  async start(): Promise<void> {
    await new Promise<void>((resolveListen) => this.#server.listen(0, "127.0.0.1", resolveListen));
    const address = this.#server.address();
    if (address === null || typeof address === "string") throw new Error("mock server has no port");
    this.baseURL = `http://127.0.0.1:${address.port}/v1`;
  }

  async stop(): Promise<void> {
    await new Promise<void>((resolveClose) => this.#server.close(() => resolveClose()));
  }

  reset(): void {
    this.requests.length = 0;
  }

  toolNames(): string[] {
    const seen = new Set<string>();
    for (const request of this.requests) {
      for (const tool of request.tools ?? []) {
        const name = tool.function?.name;
        if (name !== undefined) seen.add(name);
      }
    }
    return [...seen];
  }

  toolResults(): string[] {
    const results: string[] = [];
    for (const request of this.requests) {
      for (const message of request.messages ?? []) {
        if (message.role === "tool") results.push(textOf(message));
      }
    }
    return results;
  }
}

const dir = mkdtempSync(join(tmpdir(), "lark-brain-test-"));
const libraryDir = join(dir, "library");
const dataDir = join(dir, "data");
const db = new LarkDb(join(dir, "lark.db"));
const library = new Library(libraryDir);
const model = new MockModel();
const audioCalls: string[] = [];
let brain: Brain;

function buildLibrary(): void {
  mkdirSync(libraryDir, { recursive: true });
  const simple = new DatabaseSync(join(libraryDir, "simple.db"));
  simple.exec(`CREATE TABLE pages (id INTEGER PRIMARY KEY, title TEXT NOT NULL, text TEXT NOT NULL)`);
  simple.exec(`CREATE INDEX pages_title_nocase ON pages(title COLLATE NOCASE)`);
  simple.exec(
    `CREATE VIRTUAL TABLE pages_fts USING fts5(title, text, content='pages', content_rowid='id', tokenize='porter unicode61')`,
  );
  simple
    .prepare(`INSERT INTO pages (id, title, text) VALUES (?, ?, ?)`)
    .run(1, "Otters", "Otters are playful river animals that swim and dive.");
  simple
    .prepare(`INSERT INTO pages_fts (rowid, title, text) VALUES (?, ?, ?)`)
    .run(1, "Otters", "Otters are playful river animals that swim and dive.");
  simple.close();
}

async function until(predicate: () => boolean, label: string, timeoutMs = 5000): Promise<void> {
  const start = Date.now();
  while (!predicate()) {
    if (Date.now() - start > timeoutMs) throw new Error(`timed out waiting for ${label}`);
    await new Promise((resolveWait) => setTimeout(resolveWait, 20));
  }
}

before(async () => {
  buildLibrary();
  db.upsertCompanion({ id: "cake", name: "Cake", personality: "a curious cat" });
  await model.start();
  const timers = new TimerScheduler(db, { onFire: () => undefined, minSeconds: 0.01 });
  brain = await Brain.create({
    directory: dir,
    dataDir,
    db,
    library,
    timers,
    agentFile: resolve(import.meta.dirname, "../../agents/lark.md"),
    audio: {
      metronome: async (_companionId, bpm, beats, durationS) => {
        audioCalls.push(`metronome:${bpm}:${beats}:${durationS}`);
        return { caption: `Metronome · ${bpm} BPM (${beats}/4)` };
      },
      tune: async (_companionId, tempo, style, notes) => {
        audioCalls.push(`tune:${tempo}:${style}:${notes.length}`);
        return { caption: `Tune · ${style}` };
      },
      identify: async () => {
        audioCalls.push("identify");
        return { matched: true, title: "Get Lucky", artist: "Daft Punk" };
      },
      stop: async () => {
        audioCalls.push("stop");
        return { stopped: true };
      },
    },
    provider: { id: "mock", baseURL: model.baseURL, models: ["m1", "m2", "m3"] },
    models: { primary: "mock/m1", fallback: "mock/m2", deep: "mock/m3" },
    stallMs: 1500,
  });
});

after(async () => {
  await brain.close();
  library.close();
  db.close();
  await model.stop();
  rmSync(dir, { recursive: true, force: true });
});

async function drain(generator: AsyncGenerator<{ type: "delta"; text: string }, { text: string }>): Promise<string> {
  let step = await generator.next();
  while (!step.done) step = await generator.next();
  return step.value.text;
}

test("model specs and agent markdown parse strictly", () => {
  assert.deepEqual(parseModelSpec("opencode/claude-haiku-5-5#none"), {
    providerID: "opencode",
    id: "claude-haiku-5-5",
    variant: "none",
  });
  assert.deepEqual(parseModelSpec("opencode/gemini-4-hydrogen#low"), {
    providerID: "opencode",
    id: "gemini-4-hydrogen",
    variant: "low",
  });
  assert.deepEqual(parseModelSpec("mock/m1"), { providerID: "mock", id: "m1" });
  assert.throws(() => parseModelSpec("nope"), /provider\/model/);
  assert.equal(DEFAULT_MODELS.primary, "opencode/gemini-4-hydrogen#low");
  assert.equal(DEFAULT_MODELS.fallback, "opencode/gpt-6-luna#low");
  assert.equal(DEFAULT_MODELS.deep, "opencode/gemini-4-hydrogen#high");

  const agent = parseAgentMarkdown(`---\ndescription: Companion\nmode: primary\npermissions:\n  - {action: "*", resource: "*", effect: deny}\n  - {action: lark_*, resource: "*", effect: allow}\n---\nBe playful.\n`);
  assert.equal(agent.description, "Companion");
  assert.equal(agent.system, "Be playful.");
  assert.deepEqual(agent.permissions, [
    { action: "*", resource: "*", effect: "deny" },
    { action: "lark_*", resource: "*", effect: "allow" },
  ]);
});

test("provider credentials are imported from the live OpenCode store", () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-credential-store-"));
  const path = join(dir, "opencode.db");
  const db = new DatabaseSync(path);
  db.exec("CREATE TABLE credential (id TEXT PRIMARY KEY, integration_id TEXT, label TEXT, value TEXT, active INTEGER, time_created INTEGER)");
  const insert = db.prepare("INSERT INTO credential VALUES (?, ?, ?, ?, ?, ?)");
  insert.run("cred_a", "opencode", "Default", JSON.stringify({
    type: "oauth", methodID: "zen", refresh: "r", access: "a", expires: 1_800_000_000,
  }), 1, 2);
  insert.run("cred_b", "opencode", "Old", JSON.stringify({ type: "key", key: "k" }), 0, 1);
  insert.run("cred_c", "github-copilot", "OAuth", JSON.stringify({ type: "oauth", methodID: "gh", refresh: "r", access: "a", expires: 1 }), 1, 3);
  insert.run("cred_d", "opencode", "Broken", "not json", 1, 4);
  db.close();

  const credentials = readOpenCodeCredentials(path, "opencode");
  assert.deepEqual(credentials, [{
    integrationID: "opencode",
    label: "Default",
    value: { type: "oauth", methodID: "zen", refresh: "r", access: "a", expires: 1_800_000_000 },
  }]);
  assert.deepEqual(readOpenCodeCredentials(join(dir, "missing.db"), "opencode"), []);
});

test("in-process startup, companion instruction, and streaming deltas", async () => {
  model.reset();
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  assert.equal(db.getSession("cake")?.sessionId, sessionID);
  assert.equal(await brain.sessionExists(sessionID), true);
  assert.equal(await brain.sessionExists("ses_missing_from_old_store"), false);
  const entries = await brain.sessionInstructions(sessionID);
  const companion = entries.find((entry) => entry.key === "lark.companion");
  assert.deepEqual(companion?.value, { id: "cake", name: "Cake", personality: "a curious cat" });

  const deltas: string[] = [];
  const generator = brain.runTurn({ sessionID, text: "STREAM please" });
  let step = await generator.next();
  while (!step.done) {
    deltas.push(step.value.text);
    step = await generator.next();
  }
  assert.ok(deltas.length >= 1);
  assert.equal(deltas.join(""), "Hello from the brain");
  assert.equal(step.value.text, "Hello from the brain");
  assert.ok(step.value.llmTotalMs >= 0);
  assert.ok(step.value.llmFirstMs >= 0);
  assert.ok(step.value.llmTotalMs >= step.value.llmFirstMs);
  assert.deepEqual(step.value.model, { providerID: "mock", id: "m1" });
});

test("two sessions stream concurrently from one multiplexed event subscription", async () => {
  const first = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  const second = await brain.createSession({ id: "moss", name: "Moss", personality: "a sleepy turtle" });
  const [a, b] = await Promise.all([
    drain(brain.runTurn({ sessionID: first, text: "STREAM first session" })),
    drain(brain.runTurn({ sessionID: second, text: "STREAM second session" })),
  ]);
  assert.equal(a, "Hello from the brain");
  assert.equal(b, "Hello from the brain");
});

test("native tools round trip through the model", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "TOOLSTATE now" }));
  assert.ok(model.toolNames().includes("lark_state"));
  assert.ok(model.toolResults().some((content) => content.includes("Cake")));

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "TOOLPATCH now" }));
  assert.equal(db.getCompanion("cake")?.listening, true);
  assert.ok(model.toolResults().some((content) => content.includes("listening")));

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "TOOLSTORY now" }));
  assert.equal(db.getStory("lantern")?.title, "Lantern");
  assert.ok(model.toolResults().some((content) => content.includes("lantern")));

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "TOOLLIBRARY now" }));
  assert.ok(model.toolResults().some((content) => content.includes("Otters")));

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "TOOLAUDIO now" }));
  assert.ok(audioCalls.includes("metronome:100:4:10"));
  assert.ok(model.toolResults().some((content) => content.includes("Metronome")));

  for (const name of ["lark_state", "lark_patch", "lark_story", "lark_library", "lark_emit", "lark_audio"]) {
    assert.ok(model.toolNames().includes(name), `missing tool ${name}`);
  }
});

test("strict tool validation rejects unknown fields without dropping them", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();
  const text = await drain(brain.runTurn({ sessionID, text: "BADPATCH now" }));
  assert.equal(text, "Tool finished.");
  assert.equal(db.getCompanion("cake")?.name, "Cake");
  assert.ok(model.toolResults().some((content) => content.includes("unknown field")));

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "BADAUDIO now" }));
  assert.ok(model.toolResults().some((content) => content.includes("unknown field")));
  assert.equal(audioCalls.includes("tune:100:hum:1"), false);
});

test("speculation holds a mutating patch until promotion", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();
  const handle = brain.startSpeculate({ sessionID, text: "GATEPATCH now" });
  await until(
    () => model.requests.some((request) => lastUser(request).includes("GATEPATCH")),
    "the model tool call",
  );
  await new Promise((resolveWait) => setTimeout(resolveWait, 120));
  assert.equal(db.getCompanion("cake")?.name, "Cake", "patch ran before promotion");
  assert.ok(model.toolResults().length === 0, "tool result was sent before promotion");

  handle.promote();
  const result = await handle.result;
  assert.equal(result.text, "Tool finished.");
  assert.equal(db.getCompanion("cake")?.name, "Patched");
  assert.ok(model.toolResults().some((content) => content.includes("Patched")));
  const entries = await brain.sessionInstructions(sessionID);
  assert.equal((entries.find((entry) => entry.key === "lark.companion")?.value as { name?: string }).name, "Patched");
});

test("aborting a speculation rejects and never executes the patch", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Patched", personality: "a curious cat" });
  model.reset();
  const handle = brain.startSpeculate({ sessionID, text: "GATEABORT now" });
  await until(
    () => model.requests.some((request) => lastUser(request).includes("GATEABORT")),
    "the model tool call",
  );
  await new Promise((resolveWait) => setTimeout(resolveWait, 80));
  handle.abort(new SpeculationAbortedError("cancelled by test"));
  await assert.rejects(handle.result, SpeculationAbortedError);
  await new Promise((resolveWait) => setTimeout(resolveWait, 150));
  assert.equal(db.getCompanion("cake")?.name, "Patched");
  assert.equal(db.getCompanion("cake")?.personality, "a curious cat");
});

test("aborting a speculation before its prompt never orphans or empties the next turn", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();

  const handle = brain.startSpeculate({ sessionID, text: "SPECABORT now" });
  // A commit with a mismatched transcript can land within a few milliseconds,
  // before the speculative #turn has reached oc.sessions.prompt.
  handle.abort(new SpeculationAbortedError("commit mismatch"));
  await assert.rejects(handle.result, SpeculationAbortedError);

  // The next turn waits on the speculation's chain: it must receive the real
  // reply, not an empty one left by an interrupt racing this prompt.
  const reply = await drain(brain.runTurn({ sessionID, text: "STREAM after abort" }));
  assert.equal(reply, "Hello from the brain", "the next turn was interrupted by the aborted speculation");

  assert.equal(
    model.requests.some((request) => lastUser(request).includes("SPECABORT")),
    false,
    "an aborted speculation must never send its prompt",
  );
  const messages = await brain.sessionMessages(sessionID);
  assert.equal(
    JSON.stringify(messages).includes("SPECABORT"),
    false,
    "aborted speculation left an orphan prompt in the session",
  );
});

test("a first-token stall retries onto the fallback model", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();
  const generator = brain.runTurn({ sessionID, text: "STALL please" });
  let step = await generator.next();
  let text = "";
  while (!step.done) {
    text += step.value.text;
    step = await generator.next();
  }
  const result = step.value;
  assert.equal(text, "recovered");
  assert.equal(result.text, "recovered");
  assert.deepEqual(result.model, { providerID: "mock", id: "m2" });
  assert.ok(model.requests.some((request) => request.model === "m2"));
});

test("lark.context strips stale state from earlier turns only", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();
  const first = "Speaker: in person (name unknown)\nSTREAM first\n[state: now: Sat 2026-11-28 11:35 AM (Australia/Perth) · form: cat · volume: 55%]";
  const second = "Speaker: in person (name unknown)\nSTREAM second\n[state: now: Sat 2026-11-28 11:36 AM (Australia/Perth) · form: fox · volume: 55%]";
  for await (const _delta of brain.runTurn({ sessionID, text: first })) {
    // drain
  }
  for await (const _delta of brain.runTurn({ sessionID, text: second })) {
    // drain
  }
  const request = [...model.requests]
    .reverse()
    .find((candidate) => (candidate.messages ?? []).filter((message) => message.role === "user").length >= 2);
  assert.ok(request !== undefined);
  const users = (request.messages ?? []).filter((message) => message.role === "user");
  assert.equal(users.length, 2);
  assert.ok(!textOf(users[0]).includes("[state:"), "stale state survived");
  assert.ok(!textOf(users[0]).includes("form: cat"), "stale form survived");
  assert.match(textOf(users[1]), /\[state: now: .* · form: fox · volume: 55%\]$/);
});

test("compaction strips state from every message", () => {
  const messages: unknown[] = [
    { role: "user", content: [{ type: "text", text: "a [state: x]" }] },
    { role: "assistant", content: [{ type: "text", text: "b" }] },
    { role: "user", content: [{ type: "text", text: "c [state: y]" }] },
  ];
  stripState(messages, false);
  assert.equal((messages[0] as { content: [{ text: string }] }).content[0]?.text, "a ");
  assert.equal((messages[2] as { content: [{ text: string }] }).content[0]?.text, "c ");
});

test("missing agent markdown fails fast", async () => {
  const scratch = mkdtempSync(join(tmpdir(), "lark-brain-missing-"));
  const scratchDb = new LarkDb(join(scratch, "lark.db"));
  try {
    await assert.rejects(
      Brain.create({
        directory: scratch,
        dataDir: join(scratch, "data"),
        db: scratchDb,
        agentFile: join(scratch, "agents", "lark.md"),
        provider: { id: "mock", baseURL: model.baseURL, models: ["m1"] },
        models: { primary: "mock/m1", fallback: "mock/m1", deep: "mock/m1" },
        stallMs: 0,
      }),
      /agent definition missing/,
    );
  } finally {
    scratchDb.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

test("aborting a speculation reverts its messages from the session", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  await drain(brain.runTurn({ sessionID, text: "STREAM before" }));
  model.reset();
  const handle = brain.startSpeculate({ sessionID, text: "GATEABORT now" });
  await until(
    () => model.requests.some((request) => lastUser(request).includes("GATEABORT")),
    "the speculative prompt",
  );
  const speculative = await brain.sessionMessages(sessionID);
  assert.ok(JSON.stringify(speculative).includes("GATEABORT"), "speculative prompt never reached the session");
  handle.abort(new SpeculationAbortedError("cancelled by test"));
  await assert.rejects(handle.result, SpeculationAbortedError);

  const deadline = Date.now() + 5000;
  for (;;) {
    const messages = await brain.sessionMessages(sessionID);
    if (!JSON.stringify(messages).includes("GATEABORT")) break;
    if (Date.now() > deadline) throw new Error(`aborted speculation stayed in context: ${JSON.stringify(messages)}`);
    await new Promise((resolveWait) => setTimeout(resolveWait, 20));
  }

  model.reset();
  await drain(brain.runTurn({ sessionID, text: "STREAM after" }));
  const request = model.requests.at(-1);
  assert.ok(request !== undefined);
  assert.ok(!JSON.stringify(request.messages).includes("GATEABORT"), "reverted prompt leaked into the next turn");
});

test("a tool-step boundary inserts a space between text blocks", async () => {
  const sessionID = await brain.createSession({ id: "cake", name: "Cake", personality: "a curious cat" });
  model.reset();
  const deltas: string[] = [];
  const generator = brain.runTurn({ sessionID, text: "TOOLSTEP now" });
  let step = await generator.next();
  while (!step.done) {
    deltas.push(step.value.text);
    step = await generator.next();
  }
  assert.equal(deltas.join(""), "first second");
  assert.equal(step.value.text, "first second");
});

test("a restarted brain resolves the companion from the persisted session", async () => {
  db.upsertCompanion({ id: "restart", name: "Restart", personality: "a test owl", listening: false });
  const sessionID = await brain.createSession({ id: "restart", name: "Restart", personality: "a test owl" });
  // A real restart: the OpenCode store survives, the in-memory companion map
  // does not. Only one embedded host may own the store at a time.
  await brain.close();
  const restarted = await Brain.create({
    directory: dir,
    dataDir,
    db,
    agentFile: resolve(import.meta.dirname, "../../agents/lark.md"),
    provider: { id: "mock", baseURL: model.baseURL, models: ["m1", "m2", "m3"] },
    models: { primary: "mock/m1", fallback: "mock/m2", deep: "mock/m3" },
    stallMs: 0,
    audio: {
      metronome: async () => ({ caption: "Metronome" }),
      tune: async () => ({ caption: "Tune" }),
      identify: async () => ({ matched: false, title: "", artist: "" }),
      stop: async () => ({ stopped: true }),
    },
  });
  try {
    assert.equal(restarted.companionFor(sessionID)?.id, "restart");
    db.patchCompanion("restart", { personality: "a wiser new owl" });
    db.patchCompanion("restart", { listening: false });
    model.reset();
    await drain(restarted.runTurn({ sessionID, text: "TOOLPATCH now" }));
    const instruction = (await restarted.sessionInstructions(sessionID))
      .find((entry) => entry.key === "lark.companion")?.value as { personality?: string } | undefined;
    assert.equal(instruction?.personality, "a wiser new owl");
    assert.equal(db.getCompanion("restart")?.listening, true, "lark_patch lost the companion after restart");
    assert.ok(model.toolResults().some((content) => content.includes("listening")));
  } finally {
    await restarted.close();
  }
});

test("close is idempotent and releases the host", async () => {
  await brain.close();
  await brain.close();
});
