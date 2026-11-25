// Embedded OpenCode host for the Lark brain. One in-process @opencode/sdk
// instance owns the model turns, the native tools, and the two request hooks
// (state pruning and model failover). No listeners, no external server.
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { OpenCode } from "@opencode/sdk";
import { Plugin, Model, Provider } from "@opencode/plugin";
import type { Tool } from "@opencode/schema/tool";
import type { LarkDb, CompanionPatch, TimerRow } from "./db.ts";
import type { Library, SourceChoice } from "./library.ts";
import { parseTwee, slugFromTitle, tweeFromText } from "./stories.ts";
import type { TimerScheduler } from "./timers.ts";

type Oc = Awaited<ReturnType<typeof OpenCode.create>>;
type ToolSchema = Tool.ValueSchema;
type Permission = { action: string; resource: string; effect: "allow" | "deny" | "ask" };

export interface ModelSpecs {
  primary: string;
  fallback: string;
  deep: string;
}

export const DEFAULT_MODELS: ModelSpecs = {
  primary: "opencode/claude-haiku-5-5#none",
  fallback: "opencode/gpt-6-luna#low",
  deep: "opencode/claude-sonnet-5-5#high",
};

export interface ModelRef {
  providerID: string;
  id: string;
  variant?: string;
}

export interface CompanionInstruction {
  id: string;
  name: string;
  personality: string;
}

export interface TestProvider {
  id: string;
  baseURL: string;
  apiKey?: string;
  models: readonly string[];
}

export interface BrainOptions {
  /** Project/config root handed to OpenCode; also the default agent location. */
  directory: string;
  /** Isolated XDG root; the embedded host must never read live credentials. */
  dataDir: string;
  dbPath?: string;
  agentFile?: string;
  models?: Partial<ModelSpecs>;
  /** Test/integration hook: an OpenAI-compatible provider registered in-process. */
  provider?: TestProvider;
  stallMs?: number;
  now?: () => number;
  db: LarkDb;
  library?: Library;
  timers?: TimerScheduler;
  onEmit?: (event: EmitEvent) => void;
}

export interface EmitEvent {
  to?: string;
  say?: string;
  intercom?: { from: string; text: string };
  notes?: string[];
  sfx?: string;
}

export interface TurnInput {
  sessionID: string;
  text: string;
  agent?: string;
  model?: ModelRef;
}

export interface TurnDelta {
  type: "delta";
  text: string;
}

export interface TurnResult {
  sessionID: string;
  text: string;
  model: ModelRef;
  llmFirstMs: number;
  llmTotalMs: number;
}

export interface SpeculationHandle {
  sessionID: string;
  /** Resolves after promotion/finish, rejects immediately on abort. */
  result: Promise<TurnResult>;
  promote(): void;
  abort(reason?: Error): void;
}

export class SpeculationAbortedError extends Error {
  constructor(message = "speculative turn aborted") {
    super(message);
    this.name = "SpeculationAbortedError";
  }
}

export class ToolInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ToolInputError";
  }
}

export const READ_ONLY_TOOLS = ["lark_state", "lark_library.search", "lark_library.read", "lark_library.list", "lark_story.get"] as const;
export const MUTATING_TOOLS = ["lark_patch", "lark_emit", "lark_story.create", "lark_story.update", "lark_story.delete", "lark_library.import"] as const;

// ---- agent markdown -----------------------------------------------------

export interface AgentDefinition {
  description: string;
  mode: "primary" | "subagent" | "all";
  system: string;
  permissions: Permission[];
}

export function parseAgentMarkdown(markdown: string): AgentDefinition {
  const normalized = markdown.replaceAll("\r\n", "\n");
  let frontmatter = "";
  let body = normalized.trim();
  if (normalized.startsWith("---\n")) {
    const end = normalized.indexOf("\n---", 4);
    if (end < 0) throw new Error("agent markdown has an unterminated frontmatter block");
    frontmatter = normalized.slice(4, end);
    body = normalized.slice(end + 4).trim();
  }
  const result: AgentDefinition = { description: "", mode: "primary", system: body, permissions: [] };
  for (const line of frontmatter.split("\n")) {
    const pair = /^([A-Za-z_]+):\s*(.*)$/.exec(line);
    if (pair === null) continue;
    const key = pair[1] ?? "";
    const raw = (pair[2] ?? "").trim();
    if (key === "description") result.description = unquote(raw);
    else if (key === "mode" && (raw === "primary" || raw === "subagent" || raw === "all")) result.mode = raw;
    else if (key === "permissions") {
      // Permissions are a block list of inline maps; the loop below reads them.
      continue;
    }
  }
  for (const line of frontmatter.split("\n")) {
    const match = /^\s*-\s*\{(.*)\}\s*$/.exec(line);
    if (match === null) continue;
    const fields: Record<string, string> = {};
    for (const piece of (match[1] ?? "").split(",")) {
      const colon = piece.indexOf(":");
      if (colon < 0) continue;
      fields[piece.slice(0, colon).trim()] = unquote(piece.slice(colon + 1).trim());
    }
    const action = fields.action;
    const resource = fields.resource;
    const effect = fields.effect;
    if (action === undefined || resource === undefined) continue;
    if (effect === "allow" || effect === "deny" || effect === "ask") {
      result.permissions.push({ action, resource, effect });
    }
  }
  if (result.description === "") result.description = "Playful voice and screen companion";
  if (result.system === "") throw new Error("agent markdown has no system prompt");
  return result;
}

function unquote(value: string): string {
  const trimmed = value.trim();
  if (trimmed.length >= 2 && ((trimmed.startsWith('"') && trimmed.endsWith('"')) || (trimmed.startsWith("'") && trimmed.endsWith("'")))) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}

export function readAgentDefinition(path: string): AgentDefinition {
  if (!existsSync(path)) throw new Error(`agent definition missing: ${path}`);
  return parseAgentMarkdown(readFileSync(path, "utf8"));
}

export function parseModelSpec(spec: string): ModelRef {
  const match = /^([^/#\s]+)\/([^#\s]+)(?:#([^#\s]+))?$/.exec(spec);
  if (match === null) throw new Error(`model must be provider/model or provider/model#variant: ${spec}`);
  const [, providerID, id, variant] = match;
  if (providerID === undefined || id === undefined) throw new Error(`invalid model ${spec}`);
  return variant === undefined ? { providerID, id } : { providerID, id, variant };
}

function sameModel(a: ModelRef | undefined, b: ModelRef | undefined): boolean {
  return a !== undefined && b !== undefined && a.providerID === b.providerID && a.id === b.id && a.variant === b.variant;
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

// ---- speculation gate ---------------------------------------------------

class Speculation {
  readonly sessionID: string;
  readonly #promise: Promise<void>;
  #resolve!: () => void;
  #reject!: (error: Error) => void;
  #settled = false;
  #aborted = false;
  #abortReason: Error | undefined;

  constructor(sessionID: string) {
    this.sessionID = sessionID;
    this.#promise = new Promise<void>((resolvePromise, rejectPromise) => {
      this.#resolve = resolvePromise;
      this.#reject = rejectPromise;
    });
    // Aborting with no mutating tool in flight must not become an unhandled
    // rejection; real waiters still observe it.
    void this.#promise.catch(() => undefined);
  }

  get aborted(): boolean {
    return this.#aborted;
  }

  get abortReason(): Error | undefined {
    return this.#abortReason;
  }

  promote(): void {
    if (this.#settled) return;
    this.#settled = true;
    this.#resolve();
  }

  abort(reason: Error = new SpeculationAbortedError()): void {
    if (this.#settled) return;
    this.#settled = true;
    this.#aborted = true;
    this.#abortReason = reason;
    this.#reject(reason);
  }

  async wait(signal: AbortSignal | undefined): Promise<void> {
    if (this.#aborted) throw this.#abortReason ?? new SpeculationAbortedError();
    if (signal === undefined) {
      await this.#promise;
      return;
    }
    if (signal.aborted) throw new SpeculationAbortedError("turn cancelled");
    const cancelled = new Promise<never>((_, reject) => {
      signal.addEventListener(
        "abort",
        () => reject(new SpeculationAbortedError("turn cancelled")),
        { once: true },
      );
    });
    await Promise.race([this.#promise, cancelled]);
  }
}

export class SpeculationGate {
  readonly #active = new Map<string, Speculation>();

  begin(sessionID: string): Speculation {
    if (this.#active.has(sessionID)) {
      throw new Error(`a speculative turn is already active for ${sessionID}`);
    }
    const speculation = new Speculation(sessionID);
    this.#active.set(sessionID, speculation);
    return speculation;
  }

  promote(speculation: Speculation): void {
    this.#active.delete(speculation.sessionID);
    speculation.promote();
  }

  abort(speculation: Speculation, reason?: Error): void {
    this.#active.delete(speculation.sessionID);
    speculation.abort(reason);
  }

  close(): void {
    for (const speculation of this.#active.values()) speculation.abort(new SpeculationAbortedError("brain closed"));
    this.#active.clear();
  }

  /** Mutations wait for promotion; read-only tools never call this. */
  async mutate<T>(sessionID: string | undefined, signal: AbortSignal | undefined, run: () => T | Promise<T>): Promise<T> {
    const speculation = sessionID === undefined ? undefined : this.#active.get(sessionID);
    if (speculation === undefined) return run();
    await speculation.wait(signal);
    // Abort can win the race with a promotion; never execute a side effect after it.
    if (speculation.aborted) throw speculation.abortReason ?? new SpeculationAbortedError();
    return run();
  }
}

// ---- lark.context -------------------------------------------------------

interface RequestPart {
  type?: string;
  text?: string;
}
interface RequestMessage {
  role?: string;
  content?: string | readonly RequestPart[];
}

const STATE_PATTERN = /\n?\[state:[^\]]*\]/g;

function stripMessage(message: RequestMessage): void {
  if (message.role !== "user" || !Array.isArray(message.content)) return;
  for (const part of message.content as readonly RequestPart[]) {
    if (part.type === "text" && typeof part.text === "string" && part.text.includes("[state:")) {
      part.text = part.text.replace(STATE_PATTERN, "");
    }
  }
}

export function stripState(messages: readonly unknown[], keepLast: boolean): void {
  let lastUser = -1;
  if (keepLast) {
    for (let i = messages.length - 1; i >= 0; i--) {
      const message = messages[i] as RequestMessage | undefined;
      if (message?.role === "user") {
        lastUser = i;
        break;
      }
    }
  }
  for (let i = 0; i < messages.length; i++) {
    if (i === lastUser) continue;
    stripMessage(messages[i] as RequestMessage);
  }
}

export const contextPlugin: Plugin.Plugin = Plugin.define({
  id: "lark.context",
  async setup(ctx) {
    // A tool continuation resends the current user message, so keep its state.
    await ctx.session.hook("context", (event) => stripState(event.messages, true));
    // A compaction summary must not inherit one turn's clock, form or battery.
    await ctx.session.hook("compaction", (event) => stripState(event.messages, false));
  },
});

// ---- lark.model-fallback ------------------------------------------------

function rejectedStatus(status: number): boolean {
  return status === 429 || status === 503 || status === 403;
}

export function modelFallbackPlugin(agentID: string, primary: ModelRef, fallback: ModelRef): Plugin.Plugin {
  const responses = new Map<string, { status: number; model: ModelRef }>();
  const failedOver = new Set<string>();
  return Plugin.define({
    id: "lark.model-fallback",
    async setup(ctx) {
      await ctx.session.hook("prompt", (event) => {
        failedOver.delete(event.sessionID);
        responses.delete(event.sessionID);
      });
      await ctx.session.hook("http.response", (event) => {
        if (event.kind !== "primary" || event.agent !== agentID) return;
        if (rejectedStatus(event.response.status)) {
          responses.set(event.sessionID, { status: event.response.status, model: event.model });
        } else {
          responses.delete(event.sessionID);
        }
      });
      await ctx.session.hook("retry", (event) => {
        if (event.agent !== agentID) return;
        const recorded = responses.get(event.sessionID);
        if (recorded === undefined) return;
        if (!sameModel(recorded.model, event.model) || recorded.status !== event.error.status) return;
        responses.delete(event.sessionID);
        if (sameModel(event.model, fallback) || !sameModel(event.model, primary)) {
          event.decision = { retry: false };
          return;
        }
        // One retry onto the fallback model within the same turn.
        failedOver.add(event.sessionID);
        event.decision = { retry: true, delay: 0 };
      });
      await ctx.session.hook("experimental.model.resolve", (event) => {
        if (event.kind !== "primary" || event.agent !== agentID) return;
        if (!failedOver.has(event.sessionID)) return;
        if (sameModel(event.model, primary)) event.model = { ...fallback } as typeof event.model;
      });
    },
  });
}

// ---- brain --------------------------------------------------------------

export class Brain {
  readonly #options: BrainOptions;
  readonly #db: LarkDb;
  readonly #library: Library | undefined;
  readonly #timers: TimerScheduler | undefined;
  readonly #gate = new SpeculationGate();
  readonly #speculations = new Set<Speculation>();
  readonly #companions = new Map<string, CompanionInstruction>();
  readonly #envBefore = new Map<string, string | undefined>();
  readonly #primary: ModelRef;
  readonly #fallback: ModelRef;
  readonly #deep: ModelRef;
  readonly #now: () => number;
  readonly #stallMs: number;
  #oc: Oc | undefined;
  #closed = false;

  private constructor(options: BrainOptions) {
    this.#options = options;
    this.#db = options.db;
    this.#library = options.library;
    this.#timers = options.timers;
    this.#now = options.now ?? (() => Date.now());
    this.#stallMs = options.stallMs ?? 8000;
    const models: ModelSpecs = { ...DEFAULT_MODELS, ...options.models };
    this.#primary = parseModelSpec(models.primary);
    this.#fallback = parseModelSpec(models.fallback);
    this.#deep = parseModelSpec(models.deep);
  }

  static async create(options: BrainOptions): Promise<Brain> {
    const brain = new Brain(options);
    await brain.#start();
    return brain;
  }

  get db(): LarkDb {
    return this.#db;
  }

  get primaryModel(): ModelRef {
    return { ...this.#primary };
  }

  get fallbackModel(): ModelRef {
    return { ...this.#fallback };
  }

  get deepModel(): ModelRef {
    return { ...this.#deep };
  }

  #restoreEnv(): void {
    for (const [key, value] of this.#envBefore) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    this.#envBefore.clear();
  }

  async #start(): Promise<void> {
    const options = this.#options;
    const directory = resolve(options.directory);
    const dataDir = resolve(options.dataDir);
    mkdirSync(dataDir, { recursive: true });
    mkdirSync(join(dataDir, "config"), { recursive: true });
    mkdirSync(join(dataDir, "cache"), { recursive: true });
    mkdirSync(join(dataDir, "state"), { recursive: true });
    const xdg: Record<string, string> = {
      XDG_DATA_HOME: dataDir,
      XDG_CONFIG_HOME: join(dataDir, "config"),
      XDG_CACHE_HOME: join(dataDir, "cache"),
      XDG_STATE_HOME: join(dataDir, "state"),
    };
    for (const [key, value] of Object.entries(xdg)) {
      if (!this.#envBefore.has(key)) this.#envBefore.set(key, process.env[key]);
      process.env[key] = value;
    }

    const agentFile = options.agentFile ?? resolve(import.meta.dirname, "../../agents/lark.md");
    const agent = readAgentDefinition(agentFile);
    const primary = `${this.#primary.providerID}/${this.#primary.id}${this.#primary.variant === undefined ? "" : `#${this.#primary.variant}`}`;
    const deep = `${this.#deep.providerID}/${this.#deep.id}${this.#deep.variant === undefined ? "" : `#${this.#deep.variant}`}`;

    const config = {
      $schema: "https://opencode.ai/config.json",
      model: primary,
      default_agent: "lark",
      agents: {
        lark: {
          description: agent.description,
          mode: agent.mode,
          system: agent.system,
          model: primary,
          permissions: agent.permissions,
        },
        deep: {
          description: "Deep reasoning helper for hard or multi-step requests",
          mode: "subagent",
          system: "You are the deep reasoning helper for a child's voice companion. Answer accurately and briefly; return only prose the companion can act on.",
          model: deep,
          permissions: [{ action: "*", resource: "*", effect: "allow" as const }],
        },
      },
    };

    const plugins: Plugin.Plugin[] = [];
    if (options.provider !== undefined) plugins.push(providerPlugin(options.provider));
    plugins.push(
      toolPlugin({
        db: this.#db,
        library: this.#library,
        timers: this.#timers,
        gate: this.#gate,
        emit: options.onEmit,
        companionFor: (sessionID) => this.#companions.get(sessionID)?.id,
        setCompanion: (sessionID, companion) => this.setCompanion(sessionID, companion),
      }),
      contextPlugin,
      modelFallbackPlugin("lark", this.#primary, this.#fallback),
    );

    try {
      this.#oc = await OpenCode.create({
        database: { path: options.dbPath ?? join(dataDir, "lark-brain.db") },
        config: {
          directory,
          project: false,
          content: JSON.stringify(config),
        },
        plugins,
      });
    } catch (error) {
      this.#restoreEnv();
      throw error;
    }
  }

  #requireOc(): Oc {
    if (this.#oc === undefined) throw new Error("brain is not started");
    return this.#oc;
  }

  async createSession(companion: CompanionInstruction): Promise<string> {
    const oc = this.#requireOc();
    const session = await oc.sessions.create({
      location: { directory: resolve(this.#options.directory) },
      agent: "lark",
      model: { ...this.#primary },
    });
    await this.setCompanion(session.id, companion);
    this.#db.putSession({
      companionId: companion.id,
      sessionId: session.id,
      updatedAt: this.#now(),
      contractHash: "",
      contextAfterMs: 0,
    });
    return session.id;
  }

  /** Refresh the per-session instruction entry; no `form` is ever sent. */
  async setCompanion(sessionID: string, companion: CompanionInstruction): Promise<void> {
    const value: CompanionInstruction = {
      id: companion.id.toLowerCase(),
      name: companion.name,
      personality: companion.personality,
    };
    this.#companions.set(sessionID, value);
    await this.#requireOc().experimental.session.instructions.entry.put({
      sessionID,
      key: "lark.companion",
      value,
    });
  }

  companionFor(sessionID: string): CompanionInstruction | undefined {
    return this.#companions.get(sessionID);
  }

  /** Instruction entries currently attached to a session, for diagnostics/tests. */
  async sessionInstructions(sessionID: string): Promise<Array<{ key: string; value: unknown }>> {
    return this.#requireOc().experimental.session.instructions.entry.list({ sessionID });
  }

  async *runTurn(input: TurnInput): AsyncGenerator<TurnDelta, TurnResult> {
    const generator = this.#turn(input);
    for (;;) {
      const step = await generator.next();
      if (step.done) return step.value;
      yield step.value;
    }
  }

  startSpeculate(
    input: TurnInput,
    onDelta?: (delta: string) => void,
  ): SpeculationHandle {
    const oc = this.#requireOc();
    const speculation = this.#gate.begin(input.sessionID);
    this.#speculations.add(speculation);
    const controller = new AbortController();

    const consume = async (): Promise<TurnResult> => {
      const generator = this.#turn(input, controller.signal);
      for (;;) {
        const step = await generator.next();
        if (step.done) return step.value;
        onDelta?.(step.value.text);
      }
    };

    const completed = consume().finally(() => {
      // A speculative turn that simply finishes releases its gate.
      this.#gate.promote(speculation);
    });
    void completed.catch(() => undefined);
    const aborted = new Promise<never>((_, reject) => {
      speculation.wait(undefined).catch(reject);
    });
    const result = Promise.race([completed, aborted]).finally(() => {
      this.#speculations.delete(speculation);
    });
    void result.catch(() => undefined);

    return {
      sessionID: input.sessionID,
      result,
      promote: () => {
        this.#gate.promote(speculation);
      },
      abort: (reason?: Error) => {
        this.#gate.abort(speculation, reason);
        controller.abort();
        void oc.session.interrupt({ sessionID: input.sessionID }).catch(() => undefined);
      },
    };
  }

  promote(speculation: SpeculationHandle): void {
    speculation.promote();
  }

  abort(speculation: SpeculationHandle, reason?: Error): void {
    speculation.abort(reason);
  }

  async close(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    try {
      this.#gate.close();
      for (const speculation of this.#speculations) speculation.abort(new SpeculationAbortedError("brain closed"));
      this.#speculations.clear();
      this.#timers?.stop();
    } finally {
      try {
        await this.#oc?.close();
      } finally {
        this.#oc = undefined;
        this.#restoreEnv();
      }
    }
  }

  async *#turn(input: TurnInput, signal?: AbortSignal): AsyncGenerator<TurnDelta, TurnResult> {
    const oc = this.#requireOc();
    const sessionID = input.sessionID;
    const t0 = this.#now();
    let firstAt = 0;
    let full = "";
    let current = input.model ?? this.#primary;
    let attempts = 0;

    for (;;) {
      const attemptStart = full.length;
      const ac = new AbortController();
      const forwardAbort = (): void => ac.abort();
      signal?.addEventListener("abort", forwardAbort, { once: true });
      let stalled = false;
      let firstToken = false;
      let stallTimer: ReturnType<typeof setTimeout> | undefined;
      const iterator = oc.events.subscribe({ signal: ac.signal })[Symbol.asyncIterator]();

      if (this.#stallMs > 0) {
        stallTimer = setTimeout(() => {
          if (firstToken) return;
          stalled = true;
          void oc.session.interrupt({ sessionID }).catch(() => undefined);
        }, this.#stallMs);
      }

      try {
        await oc.session.switchModel({ sessionID, model: { ...current } });
        await oc.sessions.prompt({ sessionID, text: input.text, agent: input.agent ?? "lark" });
        for (;;) {
          const step = await iterator.next();
          if (step.done) break;
          const event = step.value;
          const data = event.data as { sessionID?: string; delta?: string } | undefined;
          if (data?.sessionID !== sessionID) continue;
          if (event.type === "session.step.first-token" || event.type === "session.text.delta") {
            firstToken = true;
            if (firstAt === 0) firstAt = this.#now();
            if (stallTimer !== undefined) {
              clearTimeout(stallTimer);
              stallTimer = undefined;
            }
          }
          if (event.type === "session.text.delta" && typeof data.delta === "string" && !stalled) {
            full += data.delta;
            yield { type: "delta", text: data.delta };
          }
          if (
            event.type === "session.execution.succeeded" ||
            event.type === "session.execution.interrupted" ||
            event.type === "session.execution.failed"
          ) {
            break;
          }
        }
      } catch (error) {
        if (!ac.signal.aborted && !signal?.aborted) throw error;
      } finally {
        ac.abort();
        if (stallTimer !== undefined) clearTimeout(stallTimer);
        signal?.removeEventListener("abort", forwardAbort);
      }

      if (!signal?.aborted) {
        try {
          await oc.session.wait({ sessionID });
        } catch (error) {
          if (!stalled) throw error;
        }
      }

      if (stalled && attempts === 0 && !sameModel(current, this.#fallback)) {
        // Discard any late text from the stalled attempt; the fallback replays
        // the same turn from scratch.
        full = full.slice(0, attemptStart);
        attempts++;
        current = this.#fallback;
        continue;
      }

      const total = this.#now() - t0;
      return {
        sessionID,
        text: full,
        model: { ...current },
        llmFirstMs: firstAt === 0 ? total : firstAt - t0,
        llmTotalMs: total,
      };
    }
  }
}

export async function createBrain(options: BrainOptions): Promise<Brain> {
  return Brain.create(options);
}

// ---- provider (test hook) ----------------------------------------------

function providerPlugin(provider: TestProvider): Plugin.Plugin {
  const providerID = Provider.ID.make(provider.id);
  return Plugin.define({
    id: "lark.test-provider",
    async setup(ctx) {
      await ctx.provider.transform((editor) => {
        editor.add({
          info: {
            ...Provider.Info.empty(providerID),
            name: `Lark test provider (${provider.id})`,
            activation: "enabled",
            package: "@opencode/ai/providers/openai-compatible",
            settings: {
              baseURL: provider.baseURL,
              apiKey: provider.apiKey ?? "lark-test-key",
            },
          },
          models: provider.models.map((id) => ({
            ...Model.Info.default(providerID, Model.ID.make(id)),
            name: id,
          })),
        });
      });
    },
  });
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

function toolPlugin(deps: ToolDeps): Plugin.Plugin {
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
            out.timer = deps.timers.set({
              companionId,
              ...(raw.in_seconds === undefined ? {} : { inSeconds: optionalNumber(raw.in_seconds, "timer.in_seconds") }),
              ...(raw.at === undefined ? {} : { at: expectString(raw.at, "timer.at", 64) }),
              ...(raw.label === undefined ? {} : { label: expectString(raw.label, "timer.label", 60) }),
              ...(raw.say === undefined ? {} : { say: expectString(raw.say, "timer.say", 600) }),
              ...(raw.chime === undefined ? {} : { chime: expectString(raw.chime, "timer.chime", 16) }),
              ...(raw.notes === undefined ? {} : { notes: timerNotes(raw.notes) }),
              ...(raw.wake_agent === undefined ? {} : { wakeAgent: optionalBoolean(raw.wake_agent, "timer.wake_agent") }),
            });
          } else if (op === "update") {
            const id = expectString(raw.id, "timer.id", 64);
            out.timer = deps.timers.update(id, companionId, {
              ...(raw.in_seconds === undefined ? {} : { inSeconds: optionalNumber(raw.in_seconds, "timer.in_seconds") }),
              ...(raw.at === undefined ? {} : { at: expectString(raw.at, "timer.at", 64) }),
              ...(raw.label === undefined ? {} : { label: expectString(raw.label, "timer.label", 60) }),
              ...(raw.say === undefined ? {} : { say: expectString(raw.say, "timer.say", 600) }),
              ...(raw.chime === undefined ? {} : { chime: expectString(raw.chime, "timer.chime", 16) }),
              ...(raw.notes === undefined ? {} : { notes: timerNotes(raw.notes) }),
              ...(raw.wake_agent === undefined ? {} : { wakeAgent: optionalBoolean(raw.wake_agent, "timer.wake_agent") }),
            });
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
        deps.db.saveStory({
          id: storyId,
          title: parsed.title,
          twee,
          author,
          updatedAt: Date.now(),
          passages: parsed.passages.length,
        });
        return textResult({ id: storyId, title: parsed.title, passages: parsed.passages.length, author });
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
          const id = slugFromTitle(parsed.title);
          deps.db.saveStory({
            id,
            title: parsed.title,
            twee,
            author: loaded.author,
            updatedAt: Date.now(),
            passages: parsed.passages.length,
          });
          return textResult({ id, title: parsed.title, passages: parsed.passages.length, source: loaded.used });
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
