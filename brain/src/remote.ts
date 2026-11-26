// Scoped remote links, lazy OpenTunnel routes, and local microphone captures.
// A token is bound to one companion and one Delta Chat contact, persisted only
// as a SHA-256 hash. Tunnels are raised on first use and persisted before the
// previous token is retired, so a failed rotation never breaks a live link.
import { execFile } from "node:child_process";
import { createHash, randomBytes, timingSafeEqual } from "node:crypto";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { delimiter, join } from "node:path";
import { promisify } from "node:util";
import type { LarkDb } from "./db.ts";

const execFileAsync = promisify(execFile);

export const REMOTE_PATH = "/remote";
export const REMOTE_COOKIE = "lark_remote";
export const REMOTE_TUNNEL_TIMEOUT_MS = 90_000;
export const CAPTURE_RETENTION_MS = 14 * 24 * 60 * 60 * 1000;
export const CAPTURE_MAX_BYTES = 1 << 30;
export const CAPTURE_DONE_WINDOW_MS = 20_000;
export const CAPTURE_DEFAULT_SECONDS = 15;
export const CAPTURE_PAGE_SECONDS = 60;

export interface RemoteTokenScope {
  companionId: string;
  contact: string;
  contactName: string;
}

export interface RemoteRoute {
  companionId: string;
  routeName: string;
  url: string;
  localPort: number;
  profile: string;
  createdAt: string;
}

export interface TunnelResult {
  stdout: string;
  stderr: string;
  code: number;
}

export type TunnelRunner = (args: readonly string[]) => Promise<TunnelResult>;

export interface RemoteManagerOptions {
  db: LarkDb;
  localPort: number;
  runner?: TunnelRunner;
  bin?: string;
  now?: () => number;
  log?: (line: string) => void;
}

function tokenHash(raw: string): string {
  return createHash("sha256").update(raw).digest("hex");
}

/** The CLI's failure text, or the start of its stdout when it said nothing. */
function firstLine(...chunks: string[]): string {
  for (const chunk of chunks) {
    const trimmed = chunk.trim();
    if (trimmed !== "") return trimmed.split("\n")[0]?.trim() ?? "";
  }
  return "no output";
}

function safeEqualHex(a: string, b: string): boolean {
  const left = Buffer.from(a, "hex");
  const right = Buffer.from(b, "hex");
  return left.length === right.length && timingSafeEqual(left, right);
}

function routeKey(companionId: string): string {
  return `remote_route:${companionId.toLowerCase()}`;
}

function tokenKey(companionId: string, contact: string): string {
  return `remote_token:${companionId.toLowerCase()}:${contact.toLowerCase()}`;
}

export function defaultTunnelRunner(bin: string): TunnelRunner {
  return async (args) => {
    try {
      const result = await execFileAsync(bin, [...args], { timeout: REMOTE_TUNNEL_TIMEOUT_MS });
      return { stdout: result.stdout, stderr: result.stderr, code: 0 };
    } catch (error) {
      const value = error as { stdout?: string; stderr?: string; code?: number; message?: string };
      return {
        stdout: value.stdout ?? "",
        stderr: value.stderr ?? value.message ?? "",
        code: typeof value.code === "number" ? value.code : 1,
      };
    }
  };
}

const TUNNEL_BIN = process.platform === "win32" ? "opentunnel.exe" : "opentunnel";

/**
 * Locate the tunnel CLI. systemd user units inherit a PATH without
 * ~/.local/bin, where opentunnel installs itself, so check that directory even
 * when the caller's PATH does not list it.
 */
export function resolveTunnelBin(
  env: NodeJS.ProcessEnv = process.env,
  exists: (path: string) => boolean = existsSync,
): string | undefined {
  const explicit = (env.LARK_OPENTUNNEL_BIN ?? "").trim();
  if (explicit !== "") return explicit;
  for (const dir of (env.PATH ?? "").split(delimiter)) {
    if (dir === "") continue;
    const candidate = join(dir, TUNNEL_BIN);
    if (exists(candidate)) return candidate;
  }
  const local = join(homedir(), ".local", "bin", TUNNEL_BIN);
  return exists(local) ? local : undefined;
}

// The tunnel CLI prints a public HTTPS URL on creation. Accept only a normal
// DNS name so a malformed line cannot inject markup into the link reply.
const ROUTE_URL = /https:\/\/[a-z0-9.-]+/g;
const ROUTE_HOST = /^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{1,62}$/;

export function parseRouteOutput(raw: string): { routeName: string; url: string } | undefined {
  for (const match of raw.matchAll(ROUTE_URL)) {
    const url = match[0];
    const host = url.slice("https://".length);
    if (!ROUTE_HOST.test(host) || !host.includes(".")) continue;
    return { routeName: host.split(".")[0] ?? "", url };
  }
  return undefined;
}

interface TokenMeta {
  hash: string;
  name?: string;
}

interface StoredRoute {
  companionId?: unknown;
  companion_id?: unknown;
  routeName?: unknown;
  route_name?: unknown;
  url?: unknown;
  localPort?: unknown;
  local_port?: unknown;
  profile?: unknown;
  createdAt?: unknown;
  created_at?: unknown;
}

/**
 * v1 stored routes with snake_case fields; accept both spellings so upgraded
 * databases keep their tunnels instead of minting a new one per restart.
 * Remove the v1 names once v1 databases are no longer upgraded in place.
 */
function decodeRoute(raw: string): RemoteRoute | undefined {
  let value: StoredRoute;
  try {
    value = JSON.parse(raw) as StoredRoute;
  } catch {
    return undefined;
  }
  const companionId = value.companionId ?? value.companion_id;
  const routeName = value.routeName ?? value.route_name;
  const localPort = value.localPort ?? value.local_port;
  const createdAt = value.createdAt ?? value.created_at;
  if (typeof companionId !== "string" || companionId.trim() === "") return undefined;
  if (typeof value.url !== "string" || !value.url.startsWith("https://")) return undefined;
  if (typeof routeName !== "string" || routeName === "") return undefined;
  if (typeof value.profile !== "string" || value.profile === "") return undefined;
  if (typeof localPort !== "number" || !Number.isInteger(localPort)) return undefined;
  if (typeof createdAt !== "string") return undefined;
  return {
    companionId: companionId.trim().toLowerCase(),
    routeName,
    url: value.url,
    localPort,
    profile: value.profile,
    createdAt,
  };
}

export class RemoteError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "RemoteError";
    this.status = status;
  }
}

export class RemoteManager {
  readonly #db: LarkDb;
  readonly #localPort: number;
  readonly #runner: TunnelRunner | undefined;
  readonly #now: () => number;
  readonly #log: (line: string) => void;
  readonly #tokens = new Map<string, RemoteTokenScope>();
  readonly #routes = new Map<string, RemoteRoute>();
  readonly #locks = new Map<string, Promise<unknown>>();
  #closed = false;

  constructor(options: RemoteManagerOptions) {
    this.#db = options.db;
    this.#localPort = options.localPort;
    this.#runner = options.runner ?? (options.bin === undefined || options.bin === "" ? undefined : defaultTunnelRunner(options.bin));
    this.#now = options.now ?? (() => Date.now());
    this.#log = options.log ?? (() => undefined);
    this.#restore();
  }

  get available(): boolean {
    return this.#runner !== undefined;
  }

  #restore(): void {
    for (const key of this.#db.listMetaKeys("remote_route:%")) {
      const raw = this.#db.getMeta(key);
      if (raw === undefined) continue;
      const route = decodeRoute(raw);
      if (route === undefined) {
        this.#log(`remote: ignoring malformed ${key}`);
        continue;
      }
      if (this.#db.getCompanion(route.companionId) === undefined) continue;
      this.#routes.set(route.companionId, route);
    }
    // Links survive a brain restart: only the hash is stored, so the plaintext
    // token remains with the contact's phone and the scoped URL keeps working.
    for (const key of this.#db.listMetaKeys("remote_token:%")) {
      const raw = this.#db.getMeta(key);
      if (raw === undefined) continue;
      const rest = key.slice("remote_token:".length);
      const split = rest.indexOf(":");
      if (split <= 0) continue;
      const companionId = rest.slice(0, split);
      const contact = rest.slice(split + 1);
      const meta = this.#decodeTokenMeta(raw);
      if (meta === undefined) {
        this.#log(`remote: ignoring malformed ${key}`);
        continue;
      }
      if (typeof meta.hash !== "string" || meta.hash.length !== 64) continue;
      if (this.#db.getCompanion(companionId) === undefined) continue;
      this.#tokens.set(meta.hash, {
        companionId,
        contact,
        contactName: typeof meta.name === "string" ? meta.name : contact,
      });
    }
  }

  /**
   * v1 stored the hash as a bare string; those links keep working, only the
   * contact name is missing until the link is reissued. Remove this fallback
   * once v1 databases are no longer upgraded in place.
   */
  #decodeTokenMeta(raw: string): TokenMeta | undefined {
    if (/^[0-9a-f]{64}$/i.test(raw)) return { hash: raw };
    try {
      return JSON.parse(raw) as TokenMeta;
    } catch {
      return undefined;
    }
  }

  #withLock<T>(companionId: string, run: () => Promise<T>): Promise<T> {
    const previous = this.#locks.get(companionId) ?? Promise.resolve();
    const next = previous.then(run, run);
    this.#locks.set(
      companionId,
      next.catch(() => undefined),
    );
    return next;
  }

  authorize(raw: string): RemoteTokenScope | undefined {
    const trimmed = raw.trim();
    if (trimmed === "") return undefined;
    const hash = tokenHash(trimmed);
    for (const [stored, scope] of this.#tokens) {
      if (safeEqualHex(stored, hash)) return scope;
    }
    return undefined;
  }

  /** Persist the replacement before retiring the previous link. */
  #storeToken(companionId: string, contact: string, contactName: string, raw: string): void {
    const hash = tokenHash(raw);
    this.#db.setMeta(tokenKey(companionId, contact), JSON.stringify({ hash, name: contactName } satisfies TokenMeta));
    for (const [oldHash, scope] of this.#tokens) {
      if (scope.companionId === companionId && scope.contact === contact) this.#tokens.delete(oldHash);
    }
    this.#tokens.set(hash, { companionId, contact, contactName });
  }

  async issue(companionId: string, contact: string, contactName: string): Promise<string> {
    const id = companionId.trim().toLowerCase();
    const peer = contact.trim().toLowerCase();
    if (this.#closed) throw new RemoteError(503, "Remote controls shutting down");
    if (this.#db.getCompanion(id) === undefined) throw new RemoteError(404, "Companion not found");
    if (this.#runner === undefined) {
      throw new RemoteError(503, "Remote controls need opentunnel; install it from opentunnel.xyz");
    }
    return await this.#withLock(id, async () => {
      let route = this.#routes.get(id);
      if (route !== undefined && route.localPort !== this.#localPort) {
        route = await this.#repoint(id, route);
      }
      if (route === undefined) {
        const profile = `lark-${randomBytes(12).toString("hex")}`;
        const result = await this.#runner?.(["route", "add", `127.0.0.1:${this.#localPort}`, "--profile", profile]);
        if (result === undefined) throw new RemoteError(503, "Remote controls unavailable");
        const parsed = parseRouteOutput(result.stdout);
        if (parsed === undefined) {
          // A missing binary or a failed enable leaves no URL on stdout, and the
          // CLI's own message is the only clue; keep it in the journal.
          this.#log(`open tunnel route add for ${id}: ${firstLine(result.stderr, result.stdout)}`);
          throw new RemoteError(502, "OpenTunnel did not return a usable remote URL");
        }
        if (result.code !== 0) {
          // Route add may report a systemd enable warning after the route is up.
          this.#log(`open tunnel route add for ${id}: ${result.stderr.trim()}`);
        }
        route = {
          companionId: id,
          routeName: parsed.routeName,
          url: parsed.url,
          localPort: this.#localPort,
          profile,
          createdAt: new Date(this.#now()).toISOString(),
        };
        this.#db.setMeta(routeKey(id), JSON.stringify(route));
        this.#routes.set(id, route);
      }

      const raw = randomBytes(16).toString("base64url");
      this.#storeToken(id, peer, contactName, raw);
      return `${route.url.replace(/\/+$/, "")}${REMOTE_PATH}?token=${encodeURIComponent(raw)}`;
    });
  }

  /** Point a restored route at the port this brain listens on, keeping its URL. */
  async #repoint(companionId: string, route: RemoteRoute): Promise<RemoteRoute> {
    const result = await this.#runner?.([
      "route",
      "add",
      `127.0.0.1:${this.#localPort}`,
      "--name",
      route.routeName,
      "--profile",
      route.profile,
    ]);
    if (result === undefined) throw new RemoteError(503, "Remote controls unavailable");
    if (result.code !== 0) {
      this.#log(`open tunnel route update for ${companionId}: ${firstLine(result.stderr, result.stdout)}`);
      throw new RemoteError(502, "OpenTunnel could not update the remote route");
    }
    const updated: RemoteRoute = { ...route, localPort: this.#localPort };
    this.#db.setMeta(routeKey(companionId), JSON.stringify(updated));
    this.#routes.set(companionId, updated);
    return updated;
  }

  async issueLocal(companionId: string, contact: string, contactName: string): Promise<string> {
    const id = companionId.toLowerCase();
    const peer = contact.trim().toLowerCase();
    const raw = randomBytes(16).toString("base64url");
    this.#storeToken(id, peer, contactName, raw);
    return `${REMOTE_PATH}?token=${encodeURIComponent(raw)}`;
  }

  async revokeContact(companionId: string, contact: string): Promise<number> {
    const id = companionId.toLowerCase();
    const peer = contact.trim().toLowerCase();
    return await this.#withLock(id, async () => {
      // Delete durable storage first: a failed delete must leave the link valid.
      this.#db.deleteMeta(tokenKey(id, peer));
      for (const [hash, scope] of this.#tokens) {
        if (scope.companionId === id && scope.contact === peer) this.#tokens.delete(hash);
      }
      let left = 0;
      for (const scope of this.#tokens.values()) if (scope.companionId === id) left++;
      if (left === 0) await this.#stop(id);
      return left;
    });
  }

  async stopCompanion(companionId: string): Promise<void> {
    const id = companionId.toLowerCase();
    await this.#withLock(id, async () => {
      await this.#stop(id);
    });
  }

  async #stop(companionId: string): Promise<void> {
    const route = this.#routes.get(companionId);
    this.#routes.delete(companionId);
    for (const [hash, scope] of this.#tokens) {
      if (scope.companionId === companionId) this.#tokens.delete(hash);
    }
    if (route !== undefined && this.#runner !== undefined) {
      const remove = await this.#runner(["route", "remove", route.routeName, "--profile", route.profile]);
      if (remove.code !== 0) this.#log(`open tunnel route remove for ${companionId}: ${remove.stderr.trim()}`);
      const down = await this.#runner(["down", "--profile", route.profile]);
      if (down.code !== 0) this.#log(`open tunnel down for ${companionId}: ${down.stderr.trim()}`);
    }
    this.#db.deleteMeta(routeKey(companionId));
    for (const key of this.#db.listMetaKeys(`remote_token:${companionId}:%`)) this.#db.deleteMeta(key);
  }

  listTunnels(): Array<{ companion_id: string; url: string; route_name: string; active: boolean; contacts: number }> {
    const out: Array<{ companion_id: string; url: string; route_name: string; active: boolean; contacts: number }> = [];
    for (const [companionId, route] of this.#routes) {
      let contacts = 0;
      for (const scope of this.#tokens.values()) if (scope.companionId === companionId) contacts++;
      out.push({ companion_id: companionId, url: route.url, route_name: route.routeName, active: contacts > 0, contacts });
    }
    out.sort((a, b) => a.companion_id.localeCompare(b.companion_id));
    return out;
  }

  async close(): Promise<void> {
    this.#closed = true;
    this.#tokens.clear();
  }
}

// Capture storage and pruning live in capture.ts; re-exported for callers.
export * from "./capture.ts";
