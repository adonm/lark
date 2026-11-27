// Typed access to the Lark household database. This module owns the schema.
import { DatabaseSync, type StatementSync } from "node:sqlite";

export const SCHEMA_STATEMENTS = [
  `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS companions (id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, dc_account_id INTEGER NOT NULL DEFAULT 0, form TEXT NOT NULL DEFAULT '', portrait_id TEXT NOT NULL DEFAULT '', listening INTEGER NOT NULL DEFAULT 0, identity_version INTEGER NOT NULL DEFAULT 0, identity_synced INTEGER NOT NULL DEFAULT 0)`,
  `CREATE TABLE IF NOT EXISTS terminals (id TEXT PRIMARY KEY, kind TEXT NOT NULL, companion_id TEXT REFERENCES companions(id) ON DELETE SET NULL, ip TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL DEFAULT '')`,
  `CREATE TABLE IF NOT EXISTS terminal_prefs (terminal_id TEXT PRIMARY KEY, volume INTEGER NOT NULL DEFAULT 55)`,
  `CREATE TABLE IF NOT EXISTS sessions (companion_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL, contract_hash TEXT NOT NULL DEFAULT '', context_after_ms INTEGER NOT NULL DEFAULT 0)`,
  `CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL, channel TEXT NOT NULL DEFAULT '', peer TEXT NOT NULL DEFAULT '')`,
  `CREATE INDEX IF NOT EXISTS messages_companion_ts_id ON messages (companion_id, ts_ms, id)`,
  `CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL, PRIMARY KEY (scope, key))`,
  `CREATE TABLE IF NOT EXISTS stories (id TEXT PRIMARY KEY, title TEXT NOT NULL, twee TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, passages INTEGER NOT NULL DEFAULT 0)`,
  `CREATE TABLE IF NOT EXISTS story_progress (companion_id TEXT NOT NULL, story_id TEXT NOT NULL, passage TEXT NOT NULL, page INTEGER NOT NULL DEFAULT 0, flags_json TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL, PRIMARY KEY (companion_id, story_id))`,
  `CREATE INDEX IF NOT EXISTS story_progress_story ON story_progress (story_id)`,
  `CREATE TABLE IF NOT EXISTS timers (id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, label TEXT NOT NULL, say TEXT NOT NULL, chime TEXT NOT NULL, wake_agent INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, fire_at TEXT NOT NULL, notes_json TEXT NOT NULL DEFAULT '[]')`,
] as const;

export const SCHEMA_TABLES = [
  "companions",
  "terminals",
  "terminal_prefs",
  "sessions",
  "messages",
  "memory",
  "timers",
  "stories",
  "story_progress",
  "meta",
] as const;

export class VersionConflictError extends Error {
  readonly scope: string;
  readonly key: string;
  readonly expected: number;
  readonly actual: number;
  constructor(scope: string, key: string, expected: number, actual: number) {
    super(`memory ${scope}/${key} expected version ${expected}, found ${actual}`);
    this.name = "VersionConflictError";
    this.scope = scope;
    this.key = key;
    this.expected = expected;
    this.actual = actual;
  }
}

export interface Companion {
  id: string;
  name: string;
  configured: boolean;
  personality: string;
  voice: string;
  updatedAt: number;
  dcAccountId: number;
  form: string;
  portraitId: string;
  listening: boolean;
  identityVersion: number;
  identitySynced: boolean;
}

export interface CompanionUpsert {
  id: string;
  name: string;
  configured?: boolean;
  personality?: string;
  voice?: string;
  updatedAt?: number;
  dcAccountId?: number;
  form?: string;
  portraitId?: string;
  listening?: boolean;
  identityVersion?: number;
  identitySynced?: boolean;
}

export type CompanionPatch = Partial<Omit<Companion, "id">>;

export interface Terminal {
  id: string;
  kind: string;
  companionId: string | null;
  ip: string;
  port: number;
  lastSeen: number;
  createdAt: string;
}

export interface TerminalWithVolume extends Terminal {
  volume: number;
}

export interface StoryWithProgress {
  story: StoryRow;
  progress?: StoryProgressRow;
}

export interface SessionRow {
  companionId: string;
  sessionId: string;
  updatedAt: number;
  contractHash: string;
  contextAfterMs: number;
}

export type MessageRole = "user" | "assistant" | "system" | "tool";

export interface Message {
  id: string;
  companionId: string;
  role: MessageRole;
  sender: string;
  text: string;
  source: string;
  channel: string;
  cardTitle: string;
  tsMs: number;
  /** Lowercased Delta Chat address of the contact in this conversation. */
  peer: string;
}

export interface MessageCursor {
  beforeTs: number;
  beforeId: string;
}

export interface MessageQuery {
  companionId: string;
  limit?: number;
  cursor?: MessageCursor;
  /** Restrict to one transport, e.g. "delta" for one remote contact. */
  channel?: string;
  sender?: string;
}

export interface MessagePage {
  messages: Message[];
  hasMore: boolean;
  nextCursor?: MessageCursor;
}

export type RemoteHistoryChannel = "all" | "in_person" | "deltachat";

export interface RemoteHistoryQuery {
  companionId: string;
  channel: RemoteHistoryChannel;
  /** Lowercased Delta Chat address of the scoped contact. */
  contact: string;
  q: string;
  limit: number;
  cursor?: MessageCursor;
}

export function escapeLike(value: string): string {
  return value.replaceAll("\\", "\\\\").replaceAll("%", "\\%").replaceAll("_", "\\_");
}

export interface MemoryRow<T = unknown> {
  scope: string;
  key: string;
  value: T;
  version: number;
  updatedAt: number;
}

export interface StoryRow {
  id: string;
  title: string;
  twee: string;
  author: string;
  updatedAt: number;
  passages: number;
}

export interface StoryProgressRow {
  companionId: string;
  storyId: string;
  passage: string;
  page: number;
  flags: Record<string, boolean | number>;
  updatedAt: number;
}

export interface TimerRow {
  id: string;
  companionId: string;
  label: string;
  say: string;
  chime: string;
  wakeAgent: boolean;
  createdAt: string;
  fireAt: string;
  notes: string[];
}

export interface TimerInsert {
  id: string;
  companionId: string;
  label: string;
  say: string;
  chime: string;
  wakeAgent: boolean;
  createdAt: string;
  fireAt: string;
  notes: string[];
}

interface CompanionSqlRow {
  id: string;
  name: string;
  configured: number;
  personality: string;
  voice: string;
  updated_at: number;
  dc_account_id: number;
  form: string;
  portrait_id: string;
  listening: number;
  identity_version: number;
  identity_synced: number;
}

interface TimerSqlRow {
  id: string;
  companion_id: string;
  label: string;
  say: string;
  chime: string;
  wake_agent: number;
  created_at: string;
  fire_at: string;
  notes_json: string;
}

function flag(value: boolean | undefined, fallback = false): boolean {
  return value ?? fallback;
}

function intFlag(value: number | bigint | null): boolean {
  return Number(value ?? 0) !== 0;
}

function num(value: number | bigint | null): number {
  return Number(value ?? 0);
}

function companionFrom(row: CompanionSqlRow): Companion {
  return {
    id: row.id,
    name: row.name,
    configured: intFlag(row.configured),
    personality: row.personality,
    voice: row.voice,
    updatedAt: num(row.updated_at),
    dcAccountId: num(row.dc_account_id),
    form: row.form,
    portraitId: row.portrait_id,
    listening: intFlag(row.listening),
    identityVersion: num(row.identity_version),
    identitySynced: intFlag(row.identity_synced),
  };
}

function timerFrom(row: TimerSqlRow): TimerRow {
  let notes: string[] = [];
  try {
    const parsed: unknown = JSON.parse(row.notes_json);
    if (Array.isArray(parsed)) notes = parsed.filter((n): n is string => typeof n === "string");
  } catch {
    notes = [];
  }
  return {
    id: row.id,
    companionId: row.companion_id,
    label: row.label,
    say: row.say,
    chime: row.chime,
    wakeAgent: intFlag(row.wake_agent),
    createdAt: row.created_at,
    fireAt: row.fire_at,
    notes,
  };
}

function messageFrom(row: Record<string, unknown>): Message {
  return {
    id: String(row.id),
    companionId: String(row.companion_id),
    role: String(row.role) as MessageRole,
    sender: String(row.sender),
    text: String(row.text),
    source: String(row.source),
    channel: String(row.channel ?? ""),
    cardTitle: String(row.card_title ?? ""),
    tsMs: num(row.ts_ms as number | bigint | null),
    peer: String(row.peer ?? ""),
  };
}

function terminalFrom(row: Record<string, unknown>): Terminal {
  return {
    id: String(row.id),
    kind: String(row.kind),
    companionId: row.companion_id == null ? null : String(row.companion_id),
    ip: String(row.ip),
    port: num(row.port as number),
    lastSeen: num(row.last_seen as number),
    createdAt: String(row.created_at),
  };
}

function sessionFrom(row: Record<string, unknown>): SessionRow {
  return {
    companionId: String(row.companion_id),
    sessionId: String(row.session_id),
    updatedAt: num(row.updated_at as number),
    contractHash: String(row.contract_hash),
    contextAfterMs: num(row.context_after_ms as number),
  };
}

function storyFrom(row: Record<string, unknown>): StoryRow {
  return {
    id: String(row.id),
    title: String(row.title),
    twee: String(row.twee),
    author: String(row.author),
    updatedAt: num(row.updated_at as number),
    passages: num(row.passages as number),
  };
}

function flagsFrom(raw: unknown): Record<string, boolean | number> {
  const flags: Record<string, boolean | number> = {};
  try {
    const parsed: unknown = JSON.parse(String(raw));
    if (parsed !== null && typeof parsed === "object" && !Array.isArray(parsed)) {
      for (const [key, value] of Object.entries(parsed)) {
        if (typeof value === "boolean" || typeof value === "number") flags[key] = value;
      }
    }
  } catch {
    // A corrupt flags blob is treated as empty progress.
  }
  return flags;
}

function memoryFrom<T = unknown>(row: Record<string, unknown>): MemoryRow<T> {
  return {
    scope: String(row.scope),
    key: String(row.key),
    value: JSON.parse(String(row.value_json)) as T,
    version: num(row.version as number),
    updatedAt: num(row.updated_at as number),
  };
}

export class LarkDb {
  readonly path: string;
  readonly #db: DatabaseSync;
  // Static SQL repeats on every turn/poll. Statements keep their compiled plan
  // until close, so preparation is a Map lookup instead of parse+plan.
  readonly #statements = new Map<string, StatementSync>();
  #closed = false;

  constructor(path: string) {
    this.path = path;
    this.#db = new DatabaseSync(path, {
      enableForeignKeyConstraints: true,
      timeout: 5000,
    });
    this.#db.exec("PRAGMA journal_mode = WAL");
    this.#db.exec("PRAGMA synchronous = NORMAL");
    this.#db.exec("PRAGMA busy_timeout = 5000");
    this.#db.exec("PRAGMA foreign_keys = ON");
    this.#db.exec("PRAGMA temp_store = MEMORY");
    for (const statement of SCHEMA_STATEMENTS) this.#db.exec(statement);
  }

  #stmt(sql: string): StatementSync {
    let statement = this.#statements.get(sql);
    if (statement === undefined) {
      statement = this.#db.prepare(sql);
      this.#statements.set(sql, statement);
    }
    return statement;
  }

  /** Escape hatch for tests and migration tooling; never used by tool code. */
  raw(): DatabaseSync {
    return this.#db;
  }

  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    this.#statements.clear();
    this.#db.close();
  }

  transaction<T>(fn: () => T): T {
    this.#db.exec("BEGIN IMMEDIATE");
    try {
      const result = fn();
      this.#db.exec("COMMIT");
      return result;
    } catch (error) {
      try {
        this.#db.exec("ROLLBACK");
      } catch {
        // A failed rollback means SQLite already ended the transaction.
      }
      throw error;
    }
  }

  // ---- meta -------------------------------------------------------------

  getMeta(key: string): string | undefined {
    const row = this.#stmt(`SELECT value FROM meta WHERE key = ?`).get(key);
    return row === undefined ? undefined : String(row.value);
  }

  setMeta(key: string, value: string): void {
    this.#stmt(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
      .run(key, value);
  }

  deleteMeta(key: string): void {
    this.#stmt(`DELETE FROM meta WHERE key = ?`).run(key);
  }

  /** Meta keys matching a SQL LIKE pattern, for durable retry queues. */
  listMetaKeys(pattern: string): string[] {
    return this.#stmt(`SELECT key FROM meta WHERE key LIKE ? ORDER BY key`)
      .all(pattern)
      .map((row) => String(row.key));
  }

  // ---- companions -------------------------------------------------------

  getCompanion(id: string): Companion | undefined {
    const row = this.#stmt(`SELECT * FROM companions WHERE id = ?`).get(id.toLowerCase()) as
      | CompanionSqlRow
      | undefined;
    return row === undefined ? undefined : companionFrom(row);
  }

  listCompanions(): Companion[] {
    const rows = this.#stmt(`SELECT * FROM companions ORDER BY name COLLATE NOCASE`).all() as unknown as CompanionSqlRow[];
    return rows.map(companionFrom);
  }

  upsertCompanion(input: CompanionUpsert): Companion {
    const id = input.id.toLowerCase();
    const now = input.updatedAt ?? Date.now();
    this.#stmt(
        `INSERT INTO companions
           (id, name, configured, personality, voice, updated_at, dc_account_id, form, portrait_id, listening, identity_version, identity_synced)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
           name = excluded.name,
           configured = excluded.configured,
           personality = excluded.personality,
           voice = excluded.voice,
           updated_at = excluded.updated_at,
           dc_account_id = excluded.dc_account_id,
           form = excluded.form,
           portrait_id = excluded.portrait_id,
           listening = excluded.listening,
           identity_version = excluded.identity_version,
           identity_synced = excluded.identity_synced`,
      )
      .run(
        id,
        input.name,
        Number(flag(input.configured)),
        input.personality ?? "",
        input.voice ?? "",
        now,
        input.dcAccountId ?? 0,
        input.form ?? "",
        input.portraitId ?? "",
        Number(flag(input.listening)),
        input.identityVersion ?? 0,
        Number(flag(input.identitySynced)),
      );
    const saved = this.getCompanion(id);
    if (saved === undefined) throw new Error(`companion ${id} vanished after upsert`);
    return saved;
  }

  patchCompanion(id: string, patch: CompanionPatch): Companion | undefined {
    const current = this.getCompanion(id);
    if (current === undefined) return undefined;
    const next = { ...current, ...patch, id: current.id, updatedAt: patch.updatedAt ?? Date.now() };
    return this.upsertCompanion(next);
  }

  deleteCompanion(id: string): void {
    this.#stmt(`DELETE FROM companions WHERE id = ?`).run(id.toLowerCase());
  }

  // ---- terminals --------------------------------------------------------

  getTerminal(id: string): Terminal | undefined {
    const row = this.#stmt(`SELECT * FROM terminals WHERE id = ?`).get(id);
    return row === undefined ? undefined : terminalFrom(row);
  }

  listTerminals(): Terminal[] {
    const rows = this.#stmt(`SELECT * FROM terminals ORDER BY id`).all();
    return rows.map(terminalFrom);
  }

  upsertTerminal(input: Omit<Terminal, "companionId"> & { companionId?: string | null }): Terminal {
    this.#stmt(
        `INSERT INTO terminals (id, kind, companion_id, ip, port, last_seen, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
           kind = excluded.kind,
           companion_id = excluded.companion_id,
           ip = excluded.ip,
           port = excluded.port,
           last_seen = excluded.last_seen,
           created_at = excluded.created_at`,
      )
      .run(
        input.id,
        input.kind,
        input.companionId == null ? null : input.companionId.toLowerCase(),
        input.ip,
        input.port,
        input.lastSeen,
        input.createdAt,
      );
    const saved = this.getTerminal(input.id);
    if (saved === undefined) throw new Error(`terminal ${input.id} vanished after upsert`);
    return saved;
  }

  deleteTerminal(id: string): void {
    this.#stmt(`DELETE FROM terminals WHERE id = ?`).run(id);
  }

  getTerminalVolume(terminalId: string): number {
    const row = this.#stmt(`SELECT volume FROM terminal_prefs WHERE terminal_id = ?`).get(terminalId);
    return row === undefined ? 55 : num(row.volume as number);
  }

  /** One JOIN per companion snapshot instead of a volume query per terminal. */
  listTerminalsWithVolume(companionId: string): TerminalWithVolume[] {
    const rows = this.#stmt(
      `SELECT t.*, COALESCE(p.volume, 55) AS volume
       FROM terminals t
       LEFT JOIN terminal_prefs p ON p.terminal_id = t.id
       WHERE t.companion_id = ?
       ORDER BY t.id`,
    ).all(companionId.toLowerCase());
    return rows.map((row) => ({ ...terminalFrom(row), volume: num(row.volume as number) }));
  }

  /** Volumes for live terminals that are not yet persisted in `terminals`. */
  getTerminalVolumes(terminalIds: string[]): Map<string, number> {
    const out = new Map<string, number>();
    if (terminalIds.length === 0) return out;
    const placeholders = terminalIds.map(() => "?").join(",");
    const rows = this.#stmt(`SELECT terminal_id, volume FROM terminal_prefs WHERE terminal_id IN (${placeholders})`)
      .all(...terminalIds);
    for (const row of rows) out.set(String(row.terminal_id), num(row.volume as number));
    return out;
  }

  setTerminalVolume(terminalId: string, volume: number): void {
    this.#stmt(
        `INSERT INTO terminal_prefs (terminal_id, volume) VALUES (?, ?)
         ON CONFLICT(terminal_id) DO UPDATE SET volume = excluded.volume`,
      )
      .run(terminalId, Math.max(0, Math.min(100, Math.round(volume))));
  }

  // ---- sessions ---------------------------------------------------------

  getSession(companionId: string): SessionRow | undefined {
    const row = this.#stmt(`SELECT * FROM sessions WHERE companion_id = ?`).get(companionId.toLowerCase());
    return row === undefined ? undefined : sessionFrom(row);
  }

  listSessions(): SessionRow[] {
    return this.#stmt(`SELECT * FROM sessions ORDER BY companion_id`)
      .all()
      .map(sessionFrom);
  }

  putSession(input: SessionRow): void {
    this.#stmt(
        `INSERT INTO sessions (companion_id, session_id, updated_at, contract_hash, context_after_ms)
         VALUES (?, ?, ?, ?, ?)
         ON CONFLICT(companion_id) DO UPDATE SET
           session_id = excluded.session_id,
           updated_at = excluded.updated_at,
           contract_hash = excluded.contract_hash,
           context_after_ms = excluded.context_after_ms`,
      )
      .run(
        input.companionId.toLowerCase(),
        input.sessionId,
        input.updatedAt,
        input.contractHash,
        input.contextAfterMs,
      );
  }

  deleteSession(companionId: string): void {
    this.#stmt(`DELETE FROM sessions WHERE companion_id = ?`).run(companionId.toLowerCase());
  }

  // ---- messages ---------------------------------------------------------

  appendMessage(message: Message): void {
    this.#stmt(
        `INSERT INTO messages (id, companion_id, role, sender, text, source, channel, card_title, ts_ms, peer)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
           role = excluded.role,
           sender = excluded.sender,
           text = excluded.text,
           source = excluded.source,
           channel = excluded.channel,
           card_title = excluded.card_title,
           ts_ms = excluded.ts_ms,
           peer = excluded.peer`,
      )
      .run(
        message.id,
        message.companionId.toLowerCase(),
        message.role,
        message.sender,
        message.text,
        message.source,
        message.channel,
        message.cardTitle,
        message.tsMs,
        message.peer,
      );
  }

  /**
   * One page of history, oldest to newest. Cursors are the `(ts_ms, id)` tuple
   * of the page's oldest row; the next page excludes it. `channel`/`sender`
   * scope a page to one remote contact without changing the index.
   */
  listMessages(query: MessageQuery): MessagePage {
    const limit = Math.max(1, Math.min(1000, Math.trunc(query.limit ?? 10)));
    let sql = `SELECT * FROM messages WHERE companion_id = ?`;
    const args: (string | number)[] = [query.companionId.toLowerCase()];
    if (query.channel !== undefined) {
      sql += ` AND channel = ?`;
      args.push(query.channel);
    }
    if (query.sender !== undefined) {
      sql += ` AND sender = ?`;
      args.push(query.sender);
    }
    if (query.cursor !== undefined) {
      sql += ` AND ts_ms <= ? AND (ts_ms < ? OR id < ?)`;
      args.push(query.cursor.beforeTs, query.cursor.beforeTs, query.cursor.beforeId);
    }
    sql += ` ORDER BY ts_ms DESC, id DESC LIMIT ?`;
    args.push(limit + 1);
    const rows = this.#stmt(sql).all(...args).map(messageFrom);
    const hasMore = rows.length > limit;
    const page = (hasMore ? rows.slice(0, limit) : rows).reverse();
    const oldest = page[0];
    return {
      messages: page,
      hasMore,
      ...(hasMore && oldest !== undefined
        ? { nextCursor: { beforeTs: oldest.tsMs, beforeId: oldest.id } }
        : {}),
    };
  }

  deleteMessages(companionId: string): void {
    this.#stmt(`DELETE FROM messages WHERE companion_id = ?`).run(companionId.toLowerCase());
  }

  /**
   * Remote-page history: in-person room notes plus the scoped contact's whole
   * Delta Chat conversation, including the companion's replies. Rows keep the
   * contact address in `peer` so other contacts' private chat stays hidden.
   * A row without a peer is never attributed by sender name, because two
   * contacts can share one display name.
   */
  listRemoteMessages(query: RemoteHistoryQuery): MessagePage {
    const limit = Math.max(1, Math.min(50, Math.trunc(query.limit)));
    let sql = `SELECT * FROM messages WHERE companion_id = ?`;
    const args: (string | number)[] = [query.companionId.toLowerCase()];
    const contact = query.contact.toLowerCase();
    if (query.channel === "in_person") {
      sql += ` AND channel = 'in_person'`;
    } else if (query.channel === "deltachat") {
      sql += ` AND channel = 'deltachat' AND LOWER(peer) = ?`;
      args.push(contact);
    } else {
      sql += ` AND (channel = 'in_person' OR (channel = 'deltachat' AND LOWER(peer) = ?))`;
      args.push(contact);
    }
    if (query.q !== "") {
      const pattern = `%${escapeLike(query.q.toLowerCase())}%`;
      sql += ` AND (LOWER(text) LIKE ? ESCAPE '\\' OR LOWER(sender) LIKE ? ESCAPE '\\')`;
      args.push(pattern, pattern);
    }
    if (query.cursor !== undefined) {
      sql += ` AND ts_ms <= ? AND (ts_ms < ? OR id < ?)`;
      args.push(query.cursor.beforeTs, query.cursor.beforeTs, query.cursor.beforeId);
    }
    sql += ` ORDER BY ts_ms DESC, id DESC LIMIT ?`;
    args.push(limit + 1);
    const rows = this.#stmt(sql).all(...args).map(messageFrom);
    const hasMore = rows.length > limit;
    const page = (hasMore ? rows.slice(0, limit) : rows).reverse();
    const oldest = page[0];
    return {
      messages: page,
      hasMore,
      ...(hasMore && oldest !== undefined
        ? { nextCursor: { beforeTs: oldest.tsMs, beforeId: oldest.id } }
        : {}),
    };
  }

  // ---- memory -----------------------------------------------------------

  getMemory<T = unknown>(scope: string, key: string): MemoryRow<T> | undefined {
    const row = this.#stmt(`SELECT * FROM memory WHERE scope = ? AND key = ?`).get(scope, key);
    return row === undefined ? undefined : memoryFrom<T>(row);
  }

  listMemory(scope: string): MemoryRow[] {
    return this.#stmt(`SELECT * FROM memory WHERE scope = ? ORDER BY key`)
      .all(scope)
      .map((row) => memoryFrom(row));
  }

  /** Upserts with optimistic concurrency; omit `expectedVersion` to write unconditionally. */
  putMemory(input: {
    scope: string;
    key: string;
    value: unknown;
    expectedVersion?: number;
    updatedAt?: number;
  }): MemoryRow {
    return this.transaction(() => {
      const existing = this.getMemory(input.scope, input.key);
      if (input.expectedVersion !== undefined && existing !== undefined && existing.version !== input.expectedVersion) {
        throw new VersionConflictError(input.scope, input.key, input.expectedVersion, existing.version);
      }
      if (input.expectedVersion !== undefined && existing === undefined && input.expectedVersion !== 0) {
        throw new VersionConflictError(input.scope, input.key, input.expectedVersion, 0);
      }
      const version = (existing?.version ?? 0) + 1;
      this.#stmt(
          `INSERT INTO memory (scope, key, value_json, version, updated_at) VALUES (?, ?, ?, ?, ?)
           ON CONFLICT(scope, key) DO UPDATE SET
             value_json = excluded.value_json,
             version = excluded.version,
             updated_at = excluded.updated_at`,
        )
        .run(input.scope, input.key, JSON.stringify(input.value), version, input.updatedAt ?? Date.now());
      const saved = this.getMemory(input.scope, input.key);
      if (saved === undefined) throw new Error("memory row vanished after write");
      return saved;
    });
  }

  deleteMemory(scope: string, key: string): void {
    this.#stmt(`DELETE FROM memory WHERE scope = ? AND key = ?`).run(scope, key);
  }

  // ---- stories ----------------------------------------------------------

  saveStory(story: StoryRow): void {
    this.#stmt(
        `INSERT INTO stories (id, title, twee, author, updated_at, passages)
         VALUES (?, ?, ?, ?, ?, ?)
         ON CONFLICT(id) DO UPDATE SET
           title = excluded.title,
           twee = excluded.twee,
           author = excluded.author,
           updated_at = excluded.updated_at,
           passages = excluded.passages`,
      )
      .run(story.id, story.title, story.twee, story.author, story.updatedAt, story.passages);
  }

  getStory(id: string): StoryRow | undefined {
    const row = this.#stmt(`SELECT * FROM stories WHERE id = ?`).get(id);
    return row === undefined ? undefined : storyFrom(row);
  }

  listStories(): StoryRow[] {
    return this.#stmt(`SELECT * FROM stories ORDER BY updated_at DESC, id`)
      .all()
      .map(storyFrom);
  }

  /** One JOIN for the remote story list instead of a progress query per story. */
  listStoriesWithProgress(companionId: string): StoryWithProgress[] {
    const rows = this.#stmt(
      `SELECT s.*,
              p.companion_id AS progress_companion_id,
              p.story_id AS progress_story_id,
              p.passage AS progress_passage,
              p.page AS progress_page,
              p.flags_json AS progress_flags_json,
              p.updated_at AS progress_updated_at
       FROM stories s
       LEFT JOIN story_progress p ON p.story_id = s.id AND p.companion_id = ?
       ORDER BY s.updated_at DESC, s.id`,
    ).all(companionId.toLowerCase());
    return rows.map((row) => ({
      story: storyFrom(row),
      ...(row.progress_story_id == null
        ? {}
        : {
            progress: {
              companionId: String(row.progress_companion_id),
              storyId: String(row.progress_story_id),
              passage: String(row.progress_passage),
              page: num(row.progress_page as number),
              flags: flagsFrom(row.progress_flags_json),
              updatedAt: num(row.progress_updated_at as number),
            },
          }),
    }));
  }

  deleteStory(id: string): void {
    this.transaction(() => {
      this.#stmt(`DELETE FROM story_progress WHERE story_id = ?`).run(id);
      this.#stmt(`DELETE FROM stories WHERE id = ?`).run(id);
    });
  }

  getStoryProgress(companionId: string, storyId: string): StoryProgressRow | undefined {
    const row = this.#stmt(`SELECT * FROM story_progress WHERE companion_id = ? AND story_id = ?`)
      .get(companionId.toLowerCase(), storyId);
    if (row === undefined) return undefined;
    return {
      companionId: String(row.companion_id),
      storyId: String(row.story_id),
      passage: String(row.passage),
      page: num(row.page as number),
      flags: flagsFrom(row.flags_json),
      updatedAt: num(row.updated_at as number),
    };
  }

  saveStoryProgress(progress: StoryProgressRow): void {
    this.#stmt(
        `INSERT INTO story_progress (companion_id, story_id, passage, page, flags_json, updated_at)
         VALUES (?, ?, ?, ?, ?, ?)
         ON CONFLICT(companion_id, story_id) DO UPDATE SET
           passage = excluded.passage,
           page = excluded.page,
           flags_json = excluded.flags_json,
           updated_at = excluded.updated_at`,
      )
      .run(
        progress.companionId.toLowerCase(),
        progress.storyId,
        progress.passage,
        progress.page,
        JSON.stringify(progress.flags),
        progress.updatedAt,
      );
  }

  deleteStoryProgress(companionId: string, storyId: string): void {
    this.#stmt(`DELETE FROM story_progress WHERE companion_id = ? AND story_id = ?`)
      .run(companionId.toLowerCase(), storyId);
  }

  // ---- timers -----------------------------------------------------------

  listTimers(companionId?: string): TimerRow[] {
    const rows =
      companionId === undefined
        ? this.#stmt(`SELECT * FROM timers ORDER BY fire_at, id`).all()
        : this.#stmt(`SELECT * FROM timers WHERE companion_id = ? ORDER BY fire_at, id`).all(companionId.toLowerCase());
    return (rows as unknown as TimerSqlRow[]).map(timerFrom);
  }

  getTimer(id: string): TimerRow | undefined {
    const row = this.#stmt(`SELECT * FROM timers WHERE id = ?`).get(id) as TimerSqlRow | undefined;
    return row === undefined ? undefined : timerFrom(row);
  }

  insertTimer(timer: TimerInsert): void {
    this.#stmt(
        `INSERT INTO timers (id, companion_id, label, say, chime, wake_agent, created_at, fire_at, notes_json)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      )
      .run(
        timer.id,
        timer.companionId.toLowerCase(),
        timer.label,
        timer.say,
        timer.chime,
        Number(timer.wakeAgent),
        timer.createdAt,
        timer.fireAt,
        JSON.stringify(timer.notes),
      );
  }

  updateTimer(timer: TimerInsert): void {
    this.#stmt(
        `UPDATE timers SET label = ?, say = ?, chime = ?, wake_agent = ?, fire_at = ?, notes_json = ? WHERE id = ?`,
      )
      .run(timer.label, timer.say, timer.chime, Number(timer.wakeAgent), timer.fireAt, JSON.stringify(timer.notes), timer.id);
  }

  claimTimer(id: string): TimerRow | undefined {
    const row = this.#stmt(`DELETE FROM timers WHERE id = ? RETURNING *`).get(id) as TimerSqlRow | undefined;
    return row === undefined ? undefined : timerFrom(row);
  }

  deleteTimer(id: string): void {
    this.#stmt(`DELETE FROM timers WHERE id = ?`).run(id);
  }

  countTimers(companionId?: string): number {
    const row =
      companionId === undefined
        ? this.#stmt(`SELECT count(*) AS n FROM timers`).get()
        : this.#stmt(`SELECT count(*) AS n FROM timers WHERE companion_id = ?`).get(companionId.toLowerCase());
    return num(row?.n as number | null);
  }
}
