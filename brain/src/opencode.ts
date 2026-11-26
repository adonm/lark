// Embedded OpenCode host for the Lark brain. One in-process @opencode/sdk
// instance owns the model turns, the native tools, and the two request hooks
// (state pruning and model failover). No listeners, no external server.
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { OpenCode } from "@opencode/sdk";
import { Plugin, Model, Provider } from "@opencode/plugin";
import type { LarkDb } from "./db.ts";
import type { Library } from "./library.ts";
import type { TimerScheduler } from "./timers.ts";
import { drain, errMessage } from "./util.ts";
import {
  SpeculationAbortedError,
  SpeculationGate,
  contextPlugin,
  modelFallbackPlugin,
  sameModel,
  type Speculation,
} from "./oc_plugins.ts";
import { toolPlugin } from "./tools.ts";

export { SpeculationAbortedError, SpeculationGate, contextPlugin, modelFallbackPlugin, stripState } from "./oc_plugins.ts";
export { ToolInputError } from "./tools.ts";

type Oc = Awaited<ReturnType<typeof OpenCode.create>>;
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

export type StoredCredentialValue =
  | { type: "key"; key: string; metadata?: Record<string, unknown> }
  | { type: "oauth"; methodID: string; refresh: string; access: string; expires: number; metadata?: Record<string, unknown> };

export interface StoredCredential {
  integrationID: string;
  label: string;
  value: StoredCredentialValue;
}

export function parseStoredCredential(raw: unknown): StoredCredentialValue | undefined {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return undefined;
  const value = raw as Record<string, unknown>;
  const metadata = value.metadata !== null && typeof value.metadata === "object" && !Array.isArray(value.metadata)
    ? value.metadata as Record<string, unknown>
    : undefined;
  if (value.type === "key" && typeof value.key === "string") {
    return { type: "key", key: value.key, ...(metadata === undefined ? {} : { metadata }) };
  }
  if (
    value.type === "oauth" &&
    typeof value.methodID === "string" &&
    typeof value.refresh === "string" &&
    typeof value.access === "string" &&
    typeof value.expires === "number"
  ) {
    return {
      type: "oauth",
      methodID: value.methodID,
      refresh: value.refresh,
      access: value.access,
      expires: value.expires,
      ...(metadata === undefined ? {} : { metadata }),
    };
  }
  return undefined;
}

/**
 * Read active provider credentials from a live OpenCode store. Auth.json is
 * legacy: current OpenCode keeps OAuth logins in its SQLite database, so a v1
 * cutover would otherwise start the embedded host without the Zen credential.
 */
export function readOpenCodeCredentials(path: string, integrationID: string): StoredCredential[] {
  let db: DatabaseSync;
  try {
    db = new DatabaseSync(path, { readOnly: true });
  } catch {
    return [];
  }
  try {
    const rows = db
      .prepare("SELECT integration_id, label, value FROM credential WHERE integration_id = ? AND active = 1 ORDER BY time_created DESC")
      .all(integrationID) as Array<{ integration_id?: unknown; label?: unknown; value?: unknown }>;
    const credentials: StoredCredential[] = [];
    for (const row of rows) {
      if (typeof row.integration_id !== "string") continue;
      let raw: unknown;
      try {
        raw = JSON.parse(String(row.value));
      } catch {
        continue;
      }
      const value = parseStoredCredential(raw);
      if (value === undefined) continue;
      credentials.push({
        integrationID: row.integration_id,
        label: typeof row.label === "string" ? row.label : "Default",
        value,
      });
    }
    return credentials;
  } catch {
    return [];
  } finally {
    db.close();
  }
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
    // The user's live OpenCode data root, captured before the isolated XDG
    // overrides below. Only the opencode provider credential is read from it.
    const userDataHome = process.env.XDG_DATA_HOME ?? join(homedir(), ".local", "share");
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
    await this.#importCredentials(userDataHome);
  }

  async #importCredentials(userDataHome: string): Promise<void> {
    // Tests and local benches register their own provider; they never read the
    // operator's OpenCode store.
    if (this.#options.provider !== undefined) return;
    const oc = this.#requireOc();
    const existing: Array<{ integrationID: string }> = await oc.credential.list().catch(() => []);
    if (existing.some((credential) => credential.integrationID === "opencode")) return;
    const source = join(resolve(userDataHome), "opencode", "opencode.db");
    for (const credential of readOpenCodeCredentials(source, "opencode")) {
      await oc.credential.create({
        integrationID: credential.integrationID,
        label: credential.label,
        value: credential.value,
        activate: true,
      }).catch(() => undefined);
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

  async sessionExists(sessionID: string): Promise<boolean> {
    try {
      await this.#requireOc().sessions.get({ sessionID });
      return true;
    } catch (error) {
      // A transient failure must not discard a healthy session; only a
      // definite "not found" resets the binding.
      if (/session not found/i.test(errMessage(error))) return false;
      return true;
    }
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
    return yield* this.#turn(input);
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
      return await drain(generator, (delta) => onDelta?.(delta.text));
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
