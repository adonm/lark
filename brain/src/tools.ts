// The five native tools handed to the embedded OpenCode host.
import { Plugin } from "@opencode/plugin";
import type { Tool } from "@opencode/schema/tool";

type ToolSchema = Tool.ValueSchema;
import type { CompanionPatch, LarkDb, TimerRow } from "./db.ts";
import type { Library, SourceChoice } from "./library.ts";
import { parseTwee, slugFromTitle, tweeFromText, type Story } from "./stories.ts";
import { formatLocalTime, type TimerEdit, type TimerScheduler } from "./timers.ts";
import type { CompanionInstruction, EmitEvent } from "./opencode.ts";
import type { SpeculationGate } from "./oc_plugins.ts";
import type { TuneNote } from "./edge_client.ts";
import type { SongMatch } from "./song_id.ts";
import { clockParts } from "./util.ts";

export class ToolInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ToolInputError";
  }
}

// ---- field schemas ------------------------------------------------------

// One declaration per field drives both the JSON Schema the model sees and
// the strict runtime parser. Objects always advertise additionalProperties
// so unknown keys reach the handler; the parser then rejects them by name.
interface Field<T> {
  readonly schema: Record<string, unknown>;
  readonly required: boolean;
  parse(value: unknown, name: string): T;
  optional(): Field<T | undefined>;
}

function field<T>(schema: Record<string, unknown>, parse: (value: unknown, name: string) => T): Field<T> {
  return {
    schema,
    required: true,
    parse,
    optional(): Field<T | undefined> {
      const optionalField: Field<T | undefined> = {
        schema,
        required: false,
        parse: (value, name) => (value === undefined ? undefined : parse(value, name)),
        optional: () => optionalField,
      };
      return optionalField;
    },
  };
}

interface Limits {
  min?: number;
  max?: number;
  description?: string;
}

function rangeText(limits: Limits): string {
  if (limits.min !== undefined && limits.max !== undefined) return ` from ${limits.min} to ${limits.max}`;
  if (limits.min !== undefined) return ` at least ${limits.min}`;
  if (limits.max !== undefined) return ` at most ${limits.max}`;
  return "";
}

function expectObject(value: unknown, name: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new ToolInputError(`${name} must be an object`);
  }
  return value as Record<string, unknown>;
}

function str(limits: Limits = {}): Field<string> {
  const schema: Record<string, unknown> = { type: "string" };
  if (limits.min !== undefined) schema.minLength = limits.min;
  if (limits.max !== undefined) schema.maxLength = limits.max;
  if (limits.description !== undefined) schema.description = limits.description;
  return field(schema, (value, name) => {
    if (typeof value !== "string") throw new ToolInputError(`${name} must be a string`);
    if (limits.min !== undefined && value.length < limits.min) {
      throw new ToolInputError(`${name} must be at least ${limits.min} characters`);
    }
    if (limits.max !== undefined && value.length > limits.max) {
      throw new ToolInputError(`${name} exceeds ${limits.max} characters`);
    }
    return value;
  });
}

function finiteNumber(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isFinite(value)) throw new ToolInputError(`${name} must be a number`);
  return value;
}

function num(limits: Limits = {}): Field<number> {
  const schema: Record<string, unknown> = { type: "number" };
  if (limits.min !== undefined) schema.minimum = limits.min;
  if (limits.max !== undefined) schema.maximum = limits.max;
  if (limits.description !== undefined) schema.description = limits.description;
  return field(schema, (value, name) => {
    const number = finiteNumber(value, name);
    if ((limits.min !== undefined && number < limits.min) || (limits.max !== undefined && number > limits.max)) {
      throw new ToolInputError(`${name} must be a number${rangeText(limits)}`);
    }
    return number;
  });
}

function int(limits: Limits = {}): Field<number> {
  const schema: Record<string, unknown> = { type: "integer" };
  if (limits.min !== undefined) schema.minimum = limits.min;
  if (limits.max !== undefined) schema.maximum = limits.max;
  if (limits.description !== undefined) schema.description = limits.description;
  return field(schema, (value, name) => {
    const number = finiteNumber(value, name);
    if (!Number.isInteger(number)) throw new ToolInputError(`${name} must be an integer`);
    if ((limits.min !== undefined && number < limits.min) || (limits.max !== undefined && number > limits.max)) {
      throw new ToolInputError(`${name} must be an integer${rangeText(limits)}`);
    }
    return number;
  });
}

function bool(description?: string): Field<boolean> {
  return field({ type: "boolean", ...(description === undefined ? {} : { description }) }, (value, name) => {
    if (typeof value !== "boolean") throw new ToolInputError(`${name} must be a boolean`);
    return value;
  });
}

function oneOf<const T extends string>(values: readonly T[], description?: string): Field<T> {
  return field(
    { type: "string", enum: [...values], ...(description === undefined ? {} : { description }) },
    (value, name) => {
      if (typeof value !== "string" || !(values as readonly string[]).includes(value)) {
        throw new ToolInputError(`${name} must be one of ${values.map((entry) => JSON.stringify(entry)).join(", ")}`);
      }
      return value as T;
    },
  );
}

interface ArrayLimits {
  min?: number;
  max?: number;
  description?: string;
}

function arr<T>(item: Field<T>, limits: ArrayLimits = {}): Field<T[]> {
  const schema: Record<string, unknown> = { type: "array", items: item.schema };
  if (limits.min !== undefined) schema.minItems = limits.min;
  if (limits.max !== undefined) schema.maxItems = limits.max;
  if (limits.description !== undefined) schema.description = limits.description;
  return field(schema, (value, name) => {
    if (!Array.isArray(value)) throw new ToolInputError(`${name} must be an array`);
    if (limits.min !== undefined && value.length < limits.min) {
      throw new ToolInputError(`${name} must contain at least ${limits.min} entries`);
    }
    if (limits.max !== undefined && value.length > limits.max) {
      throw new ToolInputError(`${name} must contain at most ${limits.max} entries`);
    }
    return value.map((entry, index) => item.parse(entry, `${name}[${index}]`));
  });
}

type Shape = Record<string, Field<any>>;
type Parsed<S extends Shape> = { [K in keyof S]: ReturnType<S[K]["parse"]> };

function obj<S extends Shape>(shape: S, description?: string): Field<Parsed<S>> {
  const entries = Object.entries(shape);
  const schema: Record<string, unknown> = {
    type: "object",
    properties: Object.fromEntries(entries.map(([key, value]) => [key, value.schema])),
    required: entries.filter(([, value]) => value.required).map(([key]) => key),
    additionalProperties: true,
    ...(description === undefined ? {} : { description }),
  };
  return field(schema, (value, name) => {
    const raw = expectObject(value, name);
    for (const key of Object.keys(raw)) {
      if (!Object.hasOwn(shape, key)) {
        throw new ToolInputError(`${name} has unknown field ${JSON.stringify(key)}`);
      }
    }
    const parsed: Record<string, unknown> = {};
    for (const [key, entry] of entries) parsed[key] = entry.parse(raw[key], `${name}.${key}`);
    return parsed as Parsed<S>;
  });
}

/** Any JSON value, used for memory values whose shape is caller-defined. */
function json(): Field<unknown> {
  return field({}, (value) => value);
}

/** A single string or an array of strings, as accepted by timer/emit notes. */
function strOrList(limits: { itemMax?: number; maxItems?: number } = {}): Field<string | string[]> {
  const list = arr(str({ max: limits.itemMax ?? 512 }), { max: limits.maxItems ?? 32 });
  return field({ anyOf: [{ type: "string" }, list.schema] }, (value, name) =>
    typeof value === "string" ? value : list.parse(value, name),
  );
}

function onlyKeys(value: Record<string, unknown>, allowed: readonly string[], name: string): void {
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new ToolInputError(`${name} has unknown field ${JSON.stringify(key)}`);
  }
}

function inputSchema(schema: Field<unknown>): ToolSchema {
  return schema.schema as unknown as ToolSchema;
}

// ---- native tools -------------------------------------------------------

export interface AudioOps {
  metronome(companionId: string, bpm: number, beats: number, durationS: number): Promise<Record<string, unknown>>;
  tune(
    companionId: string,
    tempo: number,
    style: string,
    notes: TuneNote[],
    title?: string,
  ): Promise<Record<string, unknown>>;
  identify(companionId: string, seconds: number): Promise<SongMatch & Record<string, unknown>>;
  stop(companionId: string): Promise<Record<string, unknown>>;
}

export interface ToolDeps {
  db: LarkDb;
  library?: Library;
  timers?: TimerScheduler;
  gate: SpeculationGate;
  emit?: (event: EmitEvent) => void;
  audio?: AudioOps;
  companionFor: (sessionID: string) => string | undefined;
  setCompanion: (sessionID: string, companion: CompanionInstruction) => Promise<void>;
  /** Injectable clock so state reads stay deterministic in tests. */
  now?: () => number;
}

export interface NativeTool {
  name: string;
  description: string;
  input: ToolSchema;
  options: { codemode: boolean };
  execute: (input: unknown, context: { sessionID: string; signal: AbortSignal }) => Promise<{ content: string }>;
}

/** The native tools in registration order; the plugin and tests share it. */
export function nativeTools(deps: ToolDeps): NativeTool[] {
  return [
    stateTool(deps),
    patchTool(deps),
    storyTool(deps),
    libraryTool(deps),
    emitTool(deps),
    audioTool(deps),
  ];
}

export function toolPlugin(deps: ToolDeps): Plugin.Plugin {
  return Plugin.define({
    id: "lark.tools",
    async setup(ctx) {
      await ctx.tool.transform((editor) => {
        for (const tool of nativeTools(deps)) editor.add(tool);
      });
    },
  });
}

function textResult(value: unknown): { content: string } {
  return { content: JSON.stringify(value) };
}

const STATE_INPUT = obj({
  companion_id: str({ max: 64 }).optional(),
  include: arr(
    oneOf(["me", "companions", "terminals", "messages", "memory", "timers", "stories"]),
    { max: 7 },
  ).optional(),
  messages_limit: int({ min: 1, max: 30 }).optional(),
  messages_cursor: obj({
    before_ts: int(),
    before_id: str({ max: 512 }),
  }).optional(),
  channel: str({ max: 64 }).optional(),
  sender: str({ max: 128 }).optional(),
});

function stateTool(deps: ToolDeps) {
  return {
    name: "lark_state",
    description:
      "Read companion, terminal, message, memory, timer, and story state. Message pages use a (before_ts, before_id) cursor.",
    input: inputSchema(STATE_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string }) => {
      const args = STATE_INPUT.parse(input, "state");
      const companionId = args.companion_id === undefined
        ? deps.companionFor(context.sessionID)
        : args.companion_id.toLowerCase();
      if (companionId === undefined) throw new ToolInputError("companion_id is required");
      const include = args.include ?? ["me", "messages", "memory", "timers", "stories"];
      const nowDate = new Date((deps.now ?? (() => Date.now()))());
      const clock = clockParts(nowDate.getTime());
      const out: Record<string, unknown> = {
        companion_id: companionId,
        now: {
          local: formatLocalTime(nowDate),
          weekday: clock.weekday,
          date: clock.date,
          time: clock.time,
          timezone: clock.timezone,
        },
      };
      if (include.includes("me")) {
        const me = deps.db.getCompanion(companionId);
        if (me === undefined) throw new ToolInputError(`companion ${companionId} not found`);
        out.me = me;
      }
      if (include.includes("companions")) out.companions = deps.db.listCompanions();
      if (include.includes("terminals")) out.terminals = deps.db.listTerminalsWithVolume(companionId);
      if (include.includes("messages")) {
        const limit = args.messages_limit ?? 10;
        let cursor: { beforeTs: number; beforeId: string } | undefined;
        if (args.messages_cursor !== undefined) {
          const { before_ts: beforeTs, before_id: beforeId } = args.messages_cursor;
          if (beforeId === "") throw new ToolInputError("messages_cursor requires before_ts and before_id");
          cursor = { beforeTs, beforeId };
        }
        const page = deps.db.listMessages({
          companionId,
          limit,
          ...(cursor === undefined ? {} : { cursor }),
          ...(args.channel === undefined ? {} : { channel: args.channel }),
          ...(args.sender === undefined ? {} : { sender: args.sender }),
        });
        out.messages = page.messages;
        out.messages_has_more = page.hasMore;
        if (page.nextCursor !== undefined) {
          out.messages_next_cursor = { before_ts: page.nextCursor.beforeTs, before_id: page.nextCursor.beforeId };
        }
      }
      if (include.includes("memory")) {
        out.memory = [
          ...deps.db.listMemory("shared"),
          ...deps.db.listMemory(`companion:${companionId}`),
        ];
      }
      if (include.includes("timers")) {
        out.timers = deps.timers?.list(companionId) ?? deps.db.listTimers(companionId).map(timerState);
      }
      if (include.includes("stories")) {
        out.stories = deps.db.listStories().map(({ twee: _twee, ...story }) => story);
      }
      return textResult(out);
    },
  };
}

interface TimerArgs {
  in_seconds?: number;
  at?: string;
  label?: string;
  say?: string;
  chime?: "tap" | "think" | "intercom" | "celebrate";
  notes?: string | string[];
  wake_agent?: boolean;
}

/** Optional timer fields shared by set and update. */
function timerEdit(raw: TimerArgs): TimerEdit {
  return {
    ...(raw.in_seconds === undefined ? {} : { inSeconds: raw.in_seconds }),
    ...(raw.at === undefined ? {} : { at: raw.at }),
    ...(raw.label === undefined ? {} : { label: raw.label }),
    ...(raw.say === undefined ? {} : { say: raw.say }),
    ...(raw.chime === undefined ? {} : { chime: raw.chime }),
    ...(raw.notes === undefined ? {} : { notes: raw.notes }),
    ...(raw.wake_agent === undefined ? {} : { wakeAgent: raw.wake_agent }),
  };
}

/** Store a parsed story and return the shape both story tools report. */
function saveParsedStory(
  db: LarkDb,
  parsed: Story,
  twee: string,
  author: string,
  explicitId?: string,
): { id: string; title: string; passages: number; author: string } {
  const id = explicitId ?? slugFromTitle(parsed.title);
  db.saveStory({
    id,
    title: parsed.title,
    twee,
    author,
    updatedAt: Date.now(),
    passages: parsed.passages.length,
  });
  return { id, title: parsed.title, passages: parsed.passages.length, author };
}

function timerState(row: TimerRow): Record<string, unknown> {
  const fireAt = new Date(row.fireAt);
  const remaining = Math.max(0, fireAt.getTime() - Date.now());
  return {
    id: row.id,
    companion_id: row.companionId,
    label: row.label,
    say: row.say,
    chime: row.chime,
    wake_agent: row.wakeAgent,
    notes: row.notes,
    fire_at: row.fireAt,
    remaining_s: Math.ceil(remaining / 1000),
  };
}

const PATCH_INPUT = obj({
  companion_id: str({ max: 64 }).optional(),
  companion: obj({
    name: str({ max: 80 }).optional(),
    personality: str({ max: 2000 }).optional(),
    voice: str({ max: 64 }).optional(),
    listening: bool().optional(),
    form: str({ max: 80 }).optional(),
    portrait_id: str({ max: 128 }).optional(),
    configured: bool().optional(),
  }).optional(),
  memory: obj({
    op: oneOf(["set", "delete"]).optional(),
    scope: str({ max: 64 }),
    key: str({ max: 128 }),
    value: json().optional(),
    expected_version: int({ min: 0 }).optional(),
  }).optional(),
  timer: obj({
    op: oneOf(["set", "update", "cancel"]).optional(),
    id: str({ max: 64 }).optional(),
    all: bool().optional(),
    in_seconds: num({ min: 1, max: 86400 }).optional(),
    at: str({ max: 64 }).optional(),
    label: str({ max: 60 }).optional(),
    say: str({ max: 600 }).optional(),
    chime: oneOf(["tap", "think", "intercom", "celebrate"]).optional(),
    notes: strOrList().optional(),
    wake_agent: bool().optional(),
  }).optional(),
  terminal: obj({
    terminal_id: str({ max: 128 }).optional(),
    volume: int({ min: 0, max: 100 }),
  }).optional(),
});

function patchTool(deps: ToolDeps) {
  return {
    name: "lark_patch",
    description:
      "Update a companion, terminal volume, memory entry, or timer. Every field is validated; unknown fields are rejected.",
    input: inputSchema(PATCH_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = PATCH_INPUT.parse(input, "patch");
      const companionId = args.companion_id === undefined
        ? deps.companionFor(context.sessionID)
        : args.companion_id.toLowerCase();
      if (companionId === undefined) throw new ToolInputError("companion_id is required");
      if (args.companion === undefined && args.memory === undefined && args.timer === undefined && args.terminal === undefined) {
        throw new ToolInputError("patch needs one of companion, memory, timer, or terminal");
      }

      return deps.gate.mutate(context.sessionID, context.signal, async () => {
        const out: Record<string, unknown> = { companion_id: companionId };
        if (args.companion !== undefined) {
          const patch: CompanionPatch = {};
          if (args.companion.name !== undefined) {
            const name = args.companion.name.trim();
            if (name === "") throw new ToolInputError("companion.name cannot be empty");
            patch.name = name;
          }
          if (args.companion.personality !== undefined) patch.personality = args.companion.personality;
          if (args.companion.voice !== undefined) patch.voice = args.companion.voice;
          if (args.companion.listening !== undefined) patch.listening = args.companion.listening;
          if (args.companion.form !== undefined) patch.form = args.companion.form;
          if (args.companion.portrait_id !== undefined) patch.portraitId = args.companion.portrait_id;
          if (args.companion.configured !== undefined) patch.configured = args.companion.configured;
          const saved = deps.db.patchCompanion(companionId, patch);
          if (saved === undefined) throw new ToolInputError(`companion ${companionId} not found`);
          out.companion = saved;
          if ((patch.name !== undefined || patch.personality !== undefined) && deps.companionFor(context.sessionID) !== undefined) {
            await deps.setCompanion(context.sessionID, {
              id: saved.id,
              name: saved.name,
              personality: saved.personality,
            });
          }
        }

        if (args.memory !== undefined) {
          const { scope, key } = args.memory;
          if (args.memory.op === "delete") {
            deps.db.deleteMemory(scope, key);
            out.memory = { scope, key, deleted: true };
          } else {
            if (args.memory.value === undefined) throw new ToolInputError("memory.value is required for set");
            const saved = deps.db.putMemory({
              scope,
              key,
              value: args.memory.value,
              ...(args.memory.expected_version === undefined ? {} : { expectedVersion: args.memory.expected_version }),
            });
            out.memory = saved;
          }
        }

        if (args.terminal !== undefined) {
          const terminalId = args.terminal.terminal_id ?? args.companion_id ?? context.sessionID;
          deps.db.setTerminalVolume(terminalId, args.terminal.volume);
          out.terminal = { terminal_id: terminalId, volume: args.terminal.volume };
        }

        if (args.timer !== undefined) {
          if (deps.timers === undefined) throw new ToolInputError("timers are unavailable");
          const op = args.timer.op ?? "set";
          if (op === "set") {
            out.timer = deps.timers.set({ companionId, ...timerEdit(args.timer) });
          } else if (op === "update") {
            if (args.timer.id === undefined) throw new ToolInputError("timer.id is required");
            out.timer = deps.timers.update(args.timer.id, companionId, timerEdit(args.timer));
          } else {
            out.cancelled = deps.timers.cancel(companionId, {
              ...(args.timer.id === undefined ? {} : { id: args.timer.id }),
              ...(args.timer.all === undefined ? {} : { all: args.timer.all }),
            });
          }
        }
        return textResult(out);
      });
    },
  };
}

const STORY_INPUT = obj({
  op: oneOf(["create", "update", "get", "delete"]),
  id: str({ max: 64 }).optional(),
  twee: str({ max: 65536 }).optional(),
  author: str({ max: 200 }).optional(),
});

function storyTool(deps: ToolDeps) {
  return {
    name: "lark_story",
    description: "Create, update, get, or delete a Twee 3 story. Input is validated before it is stored.",
    input: inputSchema(STORY_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = STORY_INPUT.parse(input, "story");
      const op = args.op;
      if (op === "get") {
        const id = args.id;
        if (id === undefined) throw new ToolInputError("story.get requires id");
        const story = deps.db.getStory(id);
        if (story === undefined) throw new ToolInputError(`story ${id} not found`);
        const companionId = deps.companionFor(context.sessionID);
        const progress = companionId === undefined ? undefined : deps.db.getStoryProgress(companionId, id);
        return textResult({ story, ...(progress === undefined ? {} : { progress }) });
      }
      if (op === "delete") {
        const id = args.id;
        if (id === undefined) throw new ToolInputError("story.delete requires id");
        return deps.gate.mutate(context.sessionID, context.signal, () => {
          if (deps.db.getStory(id) === undefined) throw new ToolInputError(`story ${id} not found`);
          deps.db.deleteStory(id);
          return textResult({ id, deleted: true });
        });
      }
      const twee = args.twee;
      if (twee === undefined) throw new ToolInputError(`story.${op} requires twee`);
      return deps.gate.mutate(context.sessionID, context.signal, () => {
        const parsed = parseTwee(twee);
        const storyId = args.id ?? slugFromTitle(parsed.title);
        const existing = deps.db.getStory(storyId);
        if (op === "create" && existing !== undefined) throw new ToolInputError(`story ${storyId} already exists`);
        if (op === "update" && existing === undefined) throw new ToolInputError(`story ${storyId} not found`);
        const author = args.author ?? existing?.author ?? "";
        return textResult(saveParsedStory(deps.db, parsed, twee, author, storyId));
      });
    },
  };
}

const LIBRARY_INPUT = obj({
  op: oneOf(["search", "read", "list", "import"]),
  query: str({ max: 500 }).optional(),
  source: oneOf(["auto", "simple", "enwiki", "gutenberg"]).optional(),
  ref: str({ max: 500 }).optional(),
  section: int({ min: 0 }).optional(),
  limit: int({ min: 1, max: 30 }).optional(),
});

function libraryTool(deps: ToolDeps) {
  return {
    name: "lark_library",
    description:
      "Search, read, list, or import from the offline library. Search ranks Simple Wikipedia, English Wikipedia, and Gutenberg together; reads are bounded to 3200 characters per section.",
    input: inputSchema(LIBRARY_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = LIBRARY_INPUT.parse(input, "library");
      const library = deps.library;
      if (library === undefined) throw new ToolInputError("library is unavailable");
      const source: SourceChoice = args.source ?? "auto";
      if (args.op === "search") {
        if (args.query === undefined) throw new ToolInputError("query is required");
        return textResult(library.search(args.query, { source, ...(args.limit === undefined ? {} : { limit: args.limit }) }));
      }
      if (args.op === "read") {
        if (args.ref === undefined) throw new ToolInputError("ref is required");
        return textResult(library.read(source, args.ref, args.section ?? 0));
      }
      if (args.op === "list") return textResult(library.list(source, args.limit ?? 20));
      const ref = args.ref;
      if (ref === undefined) throw new ToolInputError("ref is required");
      return deps.gate.mutate(context.sessionID, context.signal, () => {
        const loaded = library.sourceText(source, ref);
        const twee = tweeFromText(loaded.title, loaded.text);
        const parsed = parseTwee(twee);
        return textResult({ ...saveParsedStory(deps.db, parsed, twee, loaded.author), source: loaded.used });
      });
    },
  };
}

const NOTE_PATTERN = /^(?:[A-Ga-g](?:#|b)?[0-8]|R|rest)$/;

function tuneNotes(notes: readonly TuneNote[] | undefined): TuneNote[] {
  if (notes === undefined) throw new ToolInputError("notes must contain 1 to 64 entries");
  return notes.map((entry, index) => {
    if (!NOTE_PATTERN.test(entry.note)) {
      throw new ToolInputError(`notes[${index}].note must look like C4, F#4, Bb3, or R`);
    }
    return { note: entry.note, beats: entry.beats };
  });
}

const AUDIO_INPUT = obj({
  op: oneOf(["metronome", "tune", "identify", "stop"]),
  bpm: int({ min: 30, max: 240 }).optional(),
  beats: int({ min: 1, max: 12 }).optional(),
  duration_s: int({ min: 5, max: 300 }).optional(),
  style: oneOf(["hum", "chime", "pluck"]).optional(),
  tempo: int({ min: 40, max: 240 }).optional(),
  notes: arr(
    obj({
      note: str({ max: 8 }),
      beats: num({ min: 0.25, max: 8 }),
    }),
    { min: 1, max: 64 },
  ).optional(),
  title: str({ max: 80 }).optional(),
  seconds: int({ min: 6, max: 10 }).optional(),
});

function audioTool(deps: ToolDeps) {
  return {
    name: "lark_audio",
    description:
      "Play a sample-exact metronome or a short hum/chime/pluck tune, identify music playing nearby, or stop current metronome/tune playback.",
    input: inputSchema(AUDIO_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = AUDIO_INPUT.parse(input, "audio");
      const fields: Record<string, readonly string[]> = {
        metronome: ["op", "bpm", "beats", "duration_s"],
        tune: ["op", "style", "tempo", "notes", "title"],
        identify: ["op", "seconds"],
        stop: ["op"],
      };
      onlyKeys(expectObject(input, "audio"), fields[args.op] ?? ["op"], "audio");
      const companionId = deps.companionFor(context.sessionID);
      if (companionId === undefined) throw new ToolInputError("companion_id is required");

      return deps.gate.mutate(context.sessionID, context.signal, async () => {
        const audio = deps.audio;
        if (audio === undefined) throw new ToolInputError("audio is unavailable");
        if (args.op === "metronome") {
          if (args.bpm === undefined) throw new ToolInputError("bpm is required");
          return textResult(
            await audio.metronome(companionId, args.bpm, args.beats ?? 4, args.duration_s ?? 120),
          );
        }
        if (args.op === "tune") {
          return textResult(
            await audio.tune(
              companionId,
              args.tempo ?? 100,
              args.style ?? "hum",
              tuneNotes(args.notes),
              args.title,
            ),
          );
        }
        if (args.op === "identify") {
          const result: Record<string, unknown> = { ...(await audio.identify(companionId, args.seconds ?? 7)) };
          delete result.terminal_id;
          delete result.seconds;
          return textResult(result);
        }
        return textResult(await audio.stop(companionId));
      });
    },
  };
}

const EMIT_INPUT = obj({
  to: str({ max: 128 }).optional(),
  say: str({ max: 1000 }).optional(),
});

function emitTool(deps: ToolDeps) {
  return {
    name: "lark_emit",
    description: "Emit a spoken line to a terminal.",
    input: inputSchema(EMIT_INPUT),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = EMIT_INPUT.parse(input, "emit");
      const event: EmitEvent = {};
      if (args.to !== undefined) event.to = args.to;
      if (args.say !== undefined) event.say = args.say;
      if (event.say === undefined) {
        throw new ToolInputError("emit needs say");
      }
      return deps.gate.mutate(context.sessionID, context.signal, () => {
        deps.emit?.(event);
        return textResult({ ok: true, ...event });
      });
    },
  };
}
