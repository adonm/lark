import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import type { AudioOps, NativeTool, ToolDeps } from "./tools.ts";
import { ToolInputError, nativeTools } from "./tools.ts";
import { LarkDb } from "./db.ts";
import { SpeculationGate } from "./oc_plugins.ts";
import type { Library } from "./library.ts";
import type { TimerScheduler } from "./timers.ts";

const TWEE =
  ":: StoryTitle\nLantern\n\n:: Start\nA small lantern waits.\n[[Pick it up->Inside]]\n\n:: Inside {\"end\": true}\nThe lantern glows.\n";

const audioCalls: string[] = [];

function deps(): ToolDeps {
  const dir = mkdtempSync(join(tmpdir(), "lark-tools-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", configured: true });
  db.saveStory({ id: "lantern", title: "Lantern", twee: TWEE, author: "", updatedAt: 1, passages: 2 });
  const library = {
    search: (query: string, options: { source?: string; limit?: number }) => ({ query, ...options, results: [] }),
    read: (source: string, ref: string, section: number) => ({ source, ref, section, text: "text" }),
    list: (source: string, limit: number) => ({ source, limit, results: [] }),
    sourceText: (source: string, ref: string) => ({
      title: "Lantern",
      ref,
      author: "Sam",
      text: "A small light.",
      used: source === "auto" ? "simple" : source,
    }),
  } as unknown as Library;
  const timers = {
    list: () => [],
    set: () => ({ id: "t1" }),
    update: (id: string) => ({ id }),
    cancel: () => ["t1"],
  } as unknown as TimerScheduler;
  const audio: AudioOps = {
    metronome: async (companionId, bpm, beats, durationS) => {
      audioCalls.push(`metronome:${companionId}:${bpm}:${beats}:${durationS}`);
      return { ok: true };
    },
    tune: async (companionId, tempo, style, notes, title) => {
      audioCalls.push(`tune:${companionId}:${tempo}:${style}:${notes.length}:${title ?? ""}`);
      return { ok: true };
    },
    identify: async (companionId, seconds) => {
      audioCalls.push(`identify:${companionId}:${seconds}`);
      return { matched: false, terminal_id: "vocat-1", seconds };
    },
    stop: async (companionId) => {
      audioCalls.push(`stop:${companionId}`);
      return { ok: true };
    },
  };
  return {
    db,
    library,
    timers,
    audio,
    gate: new SpeculationGate(),
    emit: () => undefined,
    companionFor: () => "greg",
    setCompanion: async () => undefined,
  };
}

function tool(d: ToolDeps, name: string): NativeTool {
  const found = nativeTools(d).find((entry) => entry.name === name);
  assert.ok(found !== undefined, `tool ${name} is not registered`);
  return found;
}

const VALID: Array<{ name: string; input: unknown }> = [
  {
    name: "lark_state",
    input: {
      companion_id: "Greg",
      include: ["me", "companions", "terminals", "messages", "memory", "timers", "stories"],
      messages_limit: 5,
      messages_cursor: { before_ts: 1, before_id: "m1" },
      channel: "in_person",
      sender: "Sam",
    },
  },
  {
    name: "lark_patch",
    input: {
      companion_id: "greg",
      companion: {
        name: "Gregory",
        personality: "dry",
        voice: "af_heart",
        listening: true,
        form: "a cat",
        portrait_id: "p".repeat(32),
        configured: true,
      },
      memory: { op: "set", scope: "shared", key: "home", value: { city: "here" }, expected_version: 0 },
      timer: {
        op: "set",
        in_seconds: 30,
        at: "2026-01-01T00:00:00Z",
        label: "tea",
        say: "tea is ready",
        chime: "tap",
        notes: ["steep", "pour"],
        wake_agent: true,
      },
      terminal: { terminal_id: "vocat-1", volume: 55 },
    },
  },
  { name: "lark_story", input: { op: "create", id: "little-light", twee: TWEE, author: "Sam" } },
  {
    name: "lark_library",
    input: { op: "search", query: "otters", source: "simple", ref: "Otters", section: 0, limit: 5 },
  },
  { name: "lark_emit", input: { to: "vocat-1", say: "hello", intercom: { from: "Sam", text: "hi" }, notes: ["C4", "E4"], sfx: "chime" } },
  {
    name: "lark_audio",
    input: { op: "tune", style: "hum", tempo: 100, notes: [{ note: "C4", beats: 1 }], title: "Twinkle" },
  },
];

test("native tools advertise a complete object schema", () => {
  const d = deps();
  const tools = nativeTools(d);
  assert.deepEqual(
    tools.map((entry) => entry.name),
    ["lark_state", "lark_patch", "lark_story", "lark_library", "lark_emit", "lark_audio"],
  );
  const expected: Record<string, string> = {
    lark_state: "companion_id",
    lark_patch: "",
    lark_story: "op",
    lark_library: "op",
    lark_emit: "",
    lark_audio: "op",
  };
  for (const entry of tools) {
    const schema = entry.input as { type?: string; properties?: Record<string, unknown>; required?: string[]; additionalProperties?: boolean };
    assert.equal(schema.type, "object", entry.name);
    assert.equal(schema.additionalProperties, true, entry.name);
    assert.deepEqual(schema.required, expected[entry.name] === "" ? [] : [expected[entry.name]], entry.name);
    assert.ok(Object.keys(schema.properties ?? {}).length > 0, entry.name);
    assert.equal(entry.options.codemode, false);
  }
  const audio = tool(d, "lark_audio").input as { properties: Record<string, { items?: { required?: string[] } }> };
  assert.deepEqual(audio.properties.notes?.items?.required, ["note", "beats"]);
});

test("every native tool accepts valid arguments including optional fields", async () => {
  for (const entry of VALID) {
    const d = deps();
    await tool(d, entry.name).execute(entry.input, { sessionID: "s", signal: new AbortController().signal });
  }
  // The audio table covers the remaining operations with their optional fields.
  for (const input of [
    { op: "metronome", bpm: 120, beats: 3, duration_s: 10 },
    { op: "identify", seconds: 8 },
    { op: "stop" },
  ]) {
    const d = deps();
    await tool(d, "lark_audio").execute(input, { sessionID: "s", signal: new AbortController().signal });
  }
});

test("every native tool rejects unknown keys, wrong types and out-of-bound values", async () => {
  const rejected: Array<{ name: string; input: unknown; message: RegExp }> = [
    { name: "lark_state", input: { companion_id: "greg", bogus: 1 }, message: /unknown field/ },
    { name: "lark_state", input: { companion_id: 5 }, message: /must be a string/ },
    { name: "lark_state", input: { companion_id: "greg", messages_limit: 0 }, message: /integer from 1 to 30/ },
    { name: "lark_patch", input: { companion: { name: "x", bogus: 1 } }, message: /unknown field/ },
    { name: "lark_patch", input: { terminal: { volume: "loud" } }, message: /must be a number/ },
    { name: "lark_patch", input: { terminal: { volume: 101 } }, message: /integer from 0 to 100/ },
    { name: "lark_story", input: { op: "get", bogus: 1 }, message: /unknown field/ },
    { name: "lark_story", input: { op: 5 }, message: /must be one of/ },
    { name: "lark_library", input: { op: "search", query: "x", bogus: 1 }, message: /unknown field/ },
    { name: "lark_library", input: { op: "search", query: 5 }, message: /must be a string/ },
    { name: "lark_library", input: { op: "list", limit: 31 }, message: /integer from 1 to 30/ },
    { name: "lark_audio", input: { op: "metronome", bpm: "fast" }, message: /must be a number/ },
    { name: "lark_audio", input: { op: "metronome", bpm: 300 }, message: /integer from 30 to 240/ },
    { name: "lark_audio", input: { op: "metronome", style: "hum" }, message: /unknown field/ },
    { name: "lark_emit", input: { say: "hi", bogus: 1 }, message: /unknown field/ },
    { name: "lark_emit", input: { say: 5 }, message: /must be a string/ },
    { name: "lark_emit", input: { notes: Array(33).fill("x") }, message: /at most 32/ },
  ];
  for (const entry of rejected) {
    const d = deps();
    await assert.rejects(
      tool(d, entry.name).execute(entry.input, { sessionID: "s", signal: new AbortController().signal }),
      (error: unknown) => error instanceof ToolInputError && entry.message.test(error.message),
      `${entry.name} accepted ${JSON.stringify(entry.input)}`,
    );
  }
});

test("the speculation gate classifies read-only and mutating tools", async () => {
  const cases: Array<{ name: string; input: unknown; mutating: boolean }> = [
    { name: "lark_state", input: { companion_id: "greg", include: ["me"] }, mutating: false },
    { name: "lark_story", input: { op: "get", id: "lantern" }, mutating: false },
    { name: "lark_library", input: { op: "search", query: "otters" }, mutating: false },
    { name: "lark_library", input: { op: "read", ref: "otters" }, mutating: false },
    { name: "lark_library", input: { op: "list" }, mutating: false },
    { name: "lark_patch", input: { companion: { name: "Patched" } }, mutating: true },
    { name: "lark_story", input: { op: "create", twee: TWEE }, mutating: true },
    { name: "lark_story", input: { op: "update", id: "lantern", twee: TWEE }, mutating: true },
    { name: "lark_story", input: { op: "delete", id: "lantern" }, mutating: true },
    { name: "lark_library", input: { op: "import", ref: "otters" }, mutating: true },
    { name: "lark_audio", input: { op: "metronome", bpm: 100 }, mutating: true },
    { name: "lark_audio", input: { op: "tune", notes: [{ note: "C4", beats: 1 }] }, mutating: true },
    { name: "lark_audio", input: { op: "identify" }, mutating: true },
    { name: "lark_audio", input: { op: "stop" }, mutating: true },
    { name: "lark_emit", input: { say: "hello" }, mutating: true },
  ];
  for (const entry of cases) {
    const d = deps();
    const speculation = d.gate.begin("sess");
    const pending = tool(d, entry.name).execute(entry.input, { sessionID: "sess", signal: new AbortController().signal });
    const outcome = await Promise.race([
      pending.then(() => "ran" as const, () => "ran" as const),
      new Promise<"blocked">((resolve) => setTimeout(() => resolve("blocked"), 20)),
    ]);
    d.gate.abort(speculation);
    await pending.catch(() => undefined);
    assert.equal(outcome, entry.mutating ? "blocked" : "ran", `${entry.name} gate classification`);
  }
});
