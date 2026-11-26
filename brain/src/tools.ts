// The five native tools handed to the embedded OpenCode host.
import { Plugin } from "@opencode/plugin";
import type { Tool } from "@opencode/schema/tool";

type ToolSchema = Tool.ValueSchema;
import type { CompanionPatch, LarkDb, TimerRow } from "./db.ts";
import type { Library, SourceChoice } from "./library.ts";
import { parseTwee, slugFromTitle, tweeFromText, type Story } from "./stories.ts";
import type { TimerEdit, TimerScheduler } from "./timers.ts";
import type { CompanionInstruction, EmitEvent } from "./opencode.ts";
import type { SpeculationGate } from "./oc_plugins.ts";

export class ToolInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ToolInputError";
  }
}

// ---- input validation ---------------------------------------------------

function expectObject(value: unknown, name: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new ToolInputError(`${name} must be an object`);
  }
  return value as Record<string, unknown>;
}

function onlyKeys(value: Record<string, unknown>, allowed: readonly string[], name: string): void {
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new ToolInputError(`${name} has unknown field ${JSON.stringify(key)}`);
  }
}

function expectString(value: unknown, name: string, max = 65536): string {
  if (typeof value !== "string") throw new ToolInputError(`${name} must be a string`);
  if (value.length > max) throw new ToolInputError(`${name} exceeds ${max} characters`);
  return value;
}

function optionalString(value: unknown, name: string, max = 65536): string | undefined {
  return value === undefined ? undefined : expectString(value, name, max);
}

function optionalBoolean(value: unknown, name: string): boolean | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "boolean") throw new ToolInputError(`${name} must be a boolean`);
  return value;
}

function optionalNumber(value: unknown, name: string): number | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "number" || !Number.isFinite(value)) throw new ToolInputError(`${name} must be a number`);
  return value;
}

function expectStringArray(value: unknown, name: string, max = 32): string[] {
  if (!Array.isArray(value)) throw new ToolInputError(`${name} must be an array of strings`);
  if (value.length > max) throw new ToolInputError(`${name} has too many entries`);
  return value.map((entry, index) => expectString(entry, `${name}[${index}]`, 512));
}

function jsonSchema(properties: Record<string, unknown>, required: readonly string[] = []): ToolSchema {
  return {
    type: "object",
    properties,
    required: [...required],
    // Unknown keys must reach the handler so it can reject them by name; the
    // SDK's schema decoder would otherwise silently drop them.
    additionalProperties: true,
  } as unknown as ToolSchema;
}

function schemaObject(properties: Record<string, unknown>, required: readonly string[] = []): Record<string, unknown> {
  return { type: "object", properties, required: [...required], additionalProperties: true };
}

// ---- native tools -------------------------------------------------------

interface ToolDeps {
  db: LarkDb;
  library?: Library;
  timers?: TimerScheduler;
  gate: SpeculationGate;
  emit?: (event: EmitEvent) => void;
  companionFor: (sessionID: string) => string | undefined;
  setCompanion: (sessionID: string, companion: CompanionInstruction) => Promise<void>;
}

const COMPANION_FIELDS = [
  "name",
  "personality",
  "voice",
  "listening",
  "form",
  "portrait_id",
  "configured",
] as const;

export function toolPlugin(deps: ToolDeps): Plugin.Plugin {
  return Plugin.define({
    id: "lark.tools",
    async setup(ctx) {
      await ctx.tool.transform((editor) => {
        editor.add(stateTool(deps));
        editor.add(patchTool(deps));
        editor.add(storyTool(deps));
        editor.add(libraryTool(deps));
        editor.add(emitTool(deps));
      });
    },
  });
}

function textResult(value: unknown): { content: string } {
  return { content: JSON.stringify(value) };
}

function stateTool(deps: ToolDeps) {
  return {
    name: "lark_state",
    description:
      "Read companion, terminal, message, memory, timer, and story state. Message pages use a (before_ts, before_id) cursor.",
    input: jsonSchema(
      {
        companion_id: { type: "string" },
        include: {
          type: "array",
          items: {
            type: "string",
            enum: ["me", "companions", "terminals", "messages", "memory", "timers", "stories"],
          },
        },
        messages_limit: { type: "integer", minimum: 1, maximum: 30 },
        messages_cursor: schemaObject(
          { before_ts: { type: "integer" }, before_id: { type: "string" } },
          ["before_ts", "before_id"],
        ),
        channel: { type: "string" },
        sender: { type: "string" },
      },
      ["companion_id"],
    ),
    options: { codemode: false },
    execute: async (input: unknown) => {
      const args = expectObject(input, "state");
      onlyKeys(args, ["companion_id", "include", "messages_limit", "messages_cursor", "channel", "sender"], "state");
      const companionId = expectString(args.companion_id, "companion_id", 64).toLowerCase();
      const include = args.include === undefined
        ? ["me", "messages", "memory", "timers", "stories"]
        : expectStringArray(args.include, "include", 7);
      for (const name of include) {
        if (!["me", "companions", "terminals", "messages", "memory", "timers", "stories"].includes(name)) {
          throw new ToolInputError(`include has unknown value ${JSON.stringify(name)}`);
        }
      }
      const out: Record<string, unknown> = { companion_id: companionId };
      if (include.includes("me")) {
        const me = deps.db.getCompanion(companionId);
        if (me === undefined) throw new ToolInputError(`companion ${companionId} not found`);
        out.me = me;
      }
      if (include.includes("companions")) out.companions = deps.db.listCompanions();
      if (include.includes("terminals")) out.terminals = deps.db.listTerminals();
      if (include.includes("messages")) {
        let limit = 10;
        if (args.messages_limit !== undefined) {
          const value = optionalNumber(args.messages_limit, "messages_limit");
          if (value === undefined || !Number.isInteger(value) || value < 1 || value > 30) {
            throw new ToolInputError("messages_limit must be an integer from 1 to 30");
          }
          limit = value;
        }
        let cursor: { beforeTs: number; beforeId: string } | undefined;
        if (args.messages_cursor !== undefined) {
          const raw = expectObject(args.messages_cursor, "messages_cursor");
          onlyKeys(raw, ["before_ts", "before_id"], "messages_cursor");
          const beforeTs = optionalNumber(raw.before_ts, "messages_cursor.before_ts");
          const beforeId = optionalString(raw.before_id, "messages_cursor.before_id", 512);
          if (beforeTs === undefined || !Number.isInteger(beforeTs) || beforeId === undefined || beforeId === "") {
            throw new ToolInputError("messages_cursor requires before_ts and before_id");
          }
          cursor = { beforeTs, beforeId };
        }
        const page = deps.db.listMessages({
          companionId,
          limit,
          ...(cursor === undefined ? {} : { cursor }),
          ...(args.channel === undefined ? {} : { channel: expectString(args.channel, "channel", 64) }),
          ...(args.sender === undefined ? {} : { sender: expectString(args.sender, "sender", 128) }),
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

function timerNotes(value: unknown): string | string[] {
  if (typeof value === "string") return value;
  return expectStringArray(value, "timer.notes", 32);
}

/** Optional timer fields shared by set and update. */
function timerFields(raw: Record<string, unknown>): TimerEdit {
  return {
    ...(raw.in_seconds === undefined ? {} : { inSeconds: optionalNumber(raw.in_seconds, "timer.in_seconds") }),
    ...(raw.at === undefined ? {} : { at: expectString(raw.at, "timer.at", 64) }),
    ...(raw.label === undefined ? {} : { label: expectString(raw.label, "timer.label", 60) }),
    ...(raw.say === undefined ? {} : { say: expectString(raw.say, "timer.say", 600) }),
    ...(raw.chime === undefined ? {} : { chime: expectString(raw.chime, "timer.chime", 16) }),
    ...(raw.notes === undefined ? {} : { notes: timerNotes(raw.notes) }),
    ...(raw.wake_agent === undefined ? {} : { wakeAgent: optionalBoolean(raw.wake_agent, "timer.wake_agent") }),
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

function patchTool(deps: ToolDeps) {
  return {
    name: "lark_patch",
    description:
      "Update a companion, terminal volume, memory entry, or timer. Every field is validated; unknown fields are rejected.",
    input: jsonSchema({
      companion_id: { type: "string" },
      companion: schemaObject(
        {
          name: { type: "string", maxLength: 80 },
          personality: { type: "string", maxLength: 2000 },
          voice: { type: "string", maxLength: 64 },
          listening: { type: "boolean" },
          form: { type: "string", maxLength: 80 },
          portrait_id: { type: "string", maxLength: 128 },
          configured: { type: "boolean" },
        },
        [],
      ),
      memory: schemaObject(
        {
          op: { type: "string", enum: ["set", "delete"] },
          scope: { type: "string", maxLength: 64 },
          key: { type: "string", maxLength: 128 },
          value: {},
          expected_version: { type: "integer", minimum: 0 },
        },
        ["scope", "key"],
      ),
      timer: schemaObject(
        {
          op: { type: "string", enum: ["set", "update", "cancel"] },
          id: { type: "string" },
          all: { type: "boolean" },
          in_seconds: { type: "number", minimum: 1, maximum: 86400 },
          at: { type: "string" },
          label: { type: "string", maxLength: 60 },
          say: { type: "string", maxLength: 600 },
          chime: { type: "string", enum: ["tap", "think", "intercom", "celebrate"] },
          notes: { anyOf: [{ type: "string" }, { type: "array", items: { type: "string" } }] },
          wake_agent: { type: "boolean" },
        },
        [],
      ),
      terminal: schemaObject(
        { terminal_id: { type: "string" }, volume: { type: "integer", minimum: 0, maximum: 100 } },
        ["volume"],
      ),
    }),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = expectObject(input, "patch");
      onlyKeys(args, ["companion_id", "companion", "memory", "timer", "terminal"], "patch");
      const companionId = (
        args.companion_id === undefined
          ? deps.companionFor(context.sessionID)
          : expectString(args.companion_id, "companion_id", 64).toLowerCase()
      );
      if (companionId === undefined) throw new ToolInputError("companion_id is required");
      if (args.companion === undefined && args.memory === undefined && args.timer === undefined && args.terminal === undefined) {
        throw new ToolInputError("patch needs one of companion, memory, timer, or terminal");
      }

      return deps.gate.mutate(context.sessionID, context.signal, async () => {
        const out: Record<string, unknown> = { companion_id: companionId };
        if (args.companion !== undefined) {
          const raw = expectObject(args.companion, "companion");
          onlyKeys(raw, COMPANION_FIELDS, "companion");
          const patch: CompanionPatch = {};
          if (raw.name !== undefined) {
            const name = expectString(raw.name, "companion.name", 80).trim();
            if (name === "") throw new ToolInputError("companion.name cannot be empty");
            patch.name = name;
          }
          if (raw.personality !== undefined) patch.personality = expectString(raw.personality, "companion.personality", 2000);
          if (raw.voice !== undefined) patch.voice = expectString(raw.voice, "companion.voice", 64);
          if (raw.listening !== undefined) patch.listening = optionalBoolean(raw.listening, "companion.listening");
          if (raw.form !== undefined) patch.form = expectString(raw.form, "companion.form", 80);
          if (raw.portrait_id !== undefined) patch.portraitId = expectString(raw.portrait_id, "companion.portrait_id", 128);
          if (raw.configured !== undefined) patch.configured = optionalBoolean(raw.configured, "companion.configured");
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
          const raw = expectObject(args.memory, "memory");
          onlyKeys(raw, ["op", "scope", "key", "value", "expected_version"], "memory");
          const scope = expectString(raw.scope, "memory.scope", 64);
          const key = expectString(raw.key, "memory.key", 128);
          const op = raw.op === undefined ? "set" : expectString(raw.op, "memory.op", 16);
          if (op === "delete") {
            deps.db.deleteMemory(scope, key);
            out.memory = { scope, key, deleted: true };
          } else if (op === "set") {
            if (!("value" in raw)) throw new ToolInputError("memory.value is required for set");
            const expectedVersion = optionalNumber(raw.expected_version, "memory.expected_version");
            if (expectedVersion !== undefined && (!Number.isInteger(expectedVersion) || expectedVersion < 0)) {
              throw new ToolInputError("memory.expected_version must be a non-negative integer");
            }
            const saved = deps.db.putMemory({
              scope,
              key,
              value: raw.value,
              ...(expectedVersion === undefined ? {} : { expectedVersion }),
            });
            out.memory = saved;
          } else {
            throw new ToolInputError("memory.op must be set or delete");
          }
        }

        if (args.terminal !== undefined) {
          const raw = expectObject(args.terminal, "terminal");
          onlyKeys(raw, ["terminal_id", "volume"], "terminal");
          const terminalId = raw.terminal_id === undefined
            ? expectString(args.companion_id ?? context.sessionID, "terminal_id", 128)
            : expectString(raw.terminal_id, "terminal_id", 128);
          const volume = optionalNumber(raw.volume, "terminal.volume");
          if (volume === undefined || !Number.isInteger(volume) || volume < 0 || volume > 100) {
            throw new ToolInputError("terminal.volume must be an integer from 0 to 100");
          }
          deps.db.setTerminalVolume(terminalId, volume);
          out.terminal = { terminal_id: terminalId, volume };
        }

        if (args.timer !== undefined) {
          if (deps.timers === undefined) throw new ToolInputError("timers are unavailable");
          const raw = expectObject(args.timer, "timer");
          onlyKeys(raw, ["op", "id", "all", "in_seconds", "at", "label", "say", "chime", "notes", "wake_agent"], "timer");
          const op = raw.op === undefined ? "set" : expectString(raw.op, "timer.op", 16);
          if (op === "set") {
            out.timer = deps.timers.set({ companionId, ...timerFields(raw) });
          } else if (op === "update") {
            const id = expectString(raw.id, "timer.id", 64);
            out.timer = deps.timers.update(id, companionId, timerFields(raw));
          } else if (op === "cancel") {
            const id = optionalString(raw.id, "timer.id", 64);
            const all = optionalBoolean(raw.all, "timer.all");
            out.cancelled = deps.timers.cancel(companionId, { ...(id === undefined ? {} : { id }), ...(all === undefined ? {} : { all }) });
          } else {
            throw new ToolInputError("timer.op must be set, update, or cancel");
          }
        }
        return textResult(out);
      });
    },
  };
}

function storyTool(deps: ToolDeps) {
  return {
    name: "lark_story",
    description: "Create, update, get, or delete a Twee 3 story. Input is validated before it is stored.",
    input: jsonSchema(
      {
        op: { type: "string", enum: ["create", "update", "get", "delete"] },
        id: { type: "string", maxLength: 64 },
        twee: { type: "string", maxLength: 65536 },
        author: { type: "string", maxLength: 200 },
      },
      ["op"],
    ),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = expectObject(input, "story");
      onlyKeys(args, ["op", "id", "twee", "author"], "story");
      const op = expectString(args.op, "op", 16);
      const id = optionalString(args.id, "id", 64);
      if (op === "get") {
        if (id === undefined) throw new ToolInputError("story.get requires id");
        const story = deps.db.getStory(id);
        if (story === undefined) throw new ToolInputError(`story ${id} not found`);
        const companionId = deps.companionFor(context.sessionID);
        const progress = companionId === undefined ? undefined : deps.db.getStoryProgress(companionId, id);
        return textResult({ story, ...(progress === undefined ? {} : { progress }) });
      }
      if (op === "delete") {
        if (id === undefined) throw new ToolInputError("story.delete requires id");
        return deps.gate.mutate(context.sessionID, context.signal, () => {
          if (deps.db.getStory(id) === undefined) throw new ToolInputError(`story ${id} not found`);
          deps.db.deleteStory(id);
          return textResult({ id, deleted: true });
        });
      }
      if (op !== "create" && op !== "update") throw new ToolInputError("story.op must be create, update, get, or delete");
      const twee = optionalString(args.twee, "twee", 65536);
      if (twee === undefined) throw new ToolInputError(`story.${op} requires twee`);
      return deps.gate.mutate(context.sessionID, context.signal, () => {
        const parsed = parseTwee(twee);
        const storyId = id ?? slugFromTitle(parsed.title);
        const existing = deps.db.getStory(storyId);
        if (op === "create" && existing !== undefined) throw new ToolInputError(`story ${storyId} already exists`);
        if (op === "update" && existing === undefined) throw new ToolInputError(`story ${storyId} not found`);
        const author = optionalString(args.author, "author", 200) ?? existing?.author ?? "";
        return textResult(saveParsedStory(deps.db, parsed, twee, author, storyId));
      });
    },
  };
}

function libraryTool(deps: ToolDeps) {
  return {
    name: "lark_library",
    description:
      "Search, read, list, or import from the offline library. Search ranks Simple Wikipedia, English Wikipedia, and Gutenberg together; reads are bounded to 3200 characters per section.",
    input: jsonSchema(
      {
        op: { type: "string", enum: ["search", "read", "list", "import"] },
        query: { type: "string", maxLength: 500 },
        source: { type: "string", enum: ["auto", "simple", "enwiki", "gutenberg"] },
        ref: { type: "string", maxLength: 500 },
        section: { type: "integer", minimum: 0 },
        limit: { type: "integer", minimum: 1, maximum: 30 },
      },
      ["op"],
    ),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = expectObject(input, "library");
      onlyKeys(args, ["op", "query", "source", "ref", "section", "limit"], "library");
      const library = deps.library;
      if (library === undefined) throw new ToolInputError("library is unavailable");
      const op = expectString(args.op, "op", 16);
      const source = (args.source === undefined ? "auto" : expectString(args.source, "source", 16)) as SourceChoice;
      if (!["auto", "simple", "enwiki", "gutenberg"].includes(source)) {
        throw new ToolInputError("source must be auto, simple, enwiki, or gutenberg");
      }
      const limit = optionalNumber(args.limit, "limit");
      if (limit !== undefined && (!Number.isInteger(limit) || limit < 1 || limit > 30)) {
        throw new ToolInputError("limit must be an integer from 1 to 30");
      }
      if (op === "search") {
        const query = expectString(args.query, "query", 500);
        return textResult(library.search(query, { source, ...(limit === undefined ? {} : { limit }) }));
      }
      if (op === "read") {
        const ref = expectString(args.ref, "ref", 500);
        const section = optionalNumber(args.section, "section");
        if (section !== undefined && (!Number.isInteger(section) || section < 0)) {
          throw new ToolInputError("section must be a non-negative integer");
        }
        return textResult(library.read(source, ref, section ?? 0));
      }
      if (op === "list") return textResult(library.list(source, limit ?? 20));
      if (op === "import") {
        const ref = expectString(args.ref, "ref", 500);
        return deps.gate.mutate(context.sessionID, context.signal, () => {
          const loaded = library.sourceText(source, ref);
          const twee = tweeFromText(loaded.title, loaded.text);
          const parsed = parseTwee(twee);
          return textResult({ ...saveParsedStory(deps.db, parsed, twee, loaded.author), source: loaded.used });
        });
      }
      throw new ToolInputError("library.op must be search, read, list, or import");
    },
  };
}

function emitTool(deps: ToolDeps) {
  return {
    name: "lark_emit",
    description: "Emit an intercom message, spoken line, melody, or sound effect to a terminal.",
    input: jsonSchema({
      to: { type: "string", maxLength: 128 },
      say: { type: "string", maxLength: 1000 },
      intercom: schemaObject({ from: { type: "string" }, text: { type: "string", maxLength: 1000 } }, ["from", "text"]),
      notes: { anyOf: [{ type: "string" }, { type: "array", items: { type: "string" } }] },
      sfx: { type: "string", maxLength: 64 },
    }),
    options: { codemode: false },
    execute: async (input: unknown, context: { sessionID: string; signal: AbortSignal }) => {
      const args = expectObject(input, "emit");
      onlyKeys(args, ["to", "say", "intercom", "notes", "sfx"], "emit");
      const event: EmitEvent = {};
      if (args.to !== undefined) event.to = expectString(args.to, "to", 128);
      if (args.say !== undefined) event.say = expectString(args.say, "say", 1000);
      if (args.intercom !== undefined) {
        const raw = expectObject(args.intercom, "intercom");
        onlyKeys(raw, ["from", "text"], "intercom");
        event.intercom = {
          from: expectString(raw.from, "intercom.from", 128),
          text: expectString(raw.text, "intercom.text", 1000),
        };
      }
      if (args.notes !== undefined) {
        event.notes = typeof args.notes === "string"
          ? args.notes.split(/\s+/).filter(Boolean)
          : expectStringArray(args.notes, "notes", 32);
      }
      if (args.sfx !== undefined) event.sfx = expectString(args.sfx, "sfx", 64);
      if (event.say === undefined && event.intercom === undefined && event.notes === undefined && event.sfx === undefined) {
        throw new ToolInputError("emit needs say, intercom, notes, or sfx");
      }
      return deps.gate.mutate(context.sessionID, context.signal, () => {
        deps.emit?.(event);
        return textResult({ ok: true, ...event });
      });
    },
  };
}
