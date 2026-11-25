// One node:http server for the admin API, OTA files, device-facing diagnostics
// and the scoped /remote page. No web framework: explicit method/path switches,
// strict JSON decoding, timing-safe admin auth and one SSE stream per remote
// page keep the surface small and auditable.
import { createHash, timingSafeEqual } from "node:crypto";
import { existsSync, readFileSync, statSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { extname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { LarkDb } from "./db.ts";
import type { EdgeClient, EdgeHit } from "./edge_client.ts";
import type { Router } from "./router.ts";
import { planLibrary } from "./library.ts";
import {
  CAPTURE_DEFAULT_SECONDS,
  CAPTURE_PAGE_SECONDS,
  REMOTE_COOKIE,
  REMOTE_PATH,
  RemoteError,
  type CaptureStore,
  type RemoteManager,
  type RemoteTokenScope,
} from "./remote.ts";
import type { DcService } from "./deltachat.ts";
import type { PortraitManager, PortraitSpec } from "./portraits.ts";

export const REMOTE_CSP =
  "default-src 'none'; script-src 'self'; style-src 'self' https://cdn.jsdelivr.net; img-src 'self' data:; media-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

const JSON_LIMIT = 1 << 20;
const REMOTE_POLL_MS = 750;
const REMOTE_KEEPALIVE_MS = 15_000;

const HERE = fileURLToPath(new URL(".", import.meta.url));
const DEFAULT_REMOTE_DIR = resolve(HERE, "../../lark/web/remote");

const ONE_PIXEL_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
  "base64",
);

export interface HttpOptions {
  host: string;
  port: number;
  adminToken: string;
  dataDir: string;
  db: LarkDb;
  edge: EdgeClient;
  router: Router;
  remote: RemoteManager;
  captures: CaptureStore;
  dc?: DcService;
  portraits?: PortraitManager;
  remoteDir?: string;
  opencode?: boolean;
  libraryDir?: string;
}

export interface RunningHttp {
  server: Server;
  port: number;
  close(): Promise<void>;
}

class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "HttpError";
    this.status = status;
  }
}

function fail(status: number, message: string): HttpError {
  return new HttpError(status, message);
}

function sha256(value: string): Buffer {
  return createHash("sha256").update(value).digest();
}

function tokenMatches(header: string | string[] | undefined, expected: string): boolean {
  if (expected === "" || typeof header !== "string") return false;
  const left = sha256(header);
  const right = sha256(expected);
  return left.length === right.length && timingSafeEqual(left, right);
}

function cookieValue(req: IncomingMessage, name: string): string | undefined {
  const raw = req.headers.cookie;
  if (raw === undefined) return undefined;
  for (const part of raw.split(";")) {
    const index = part.indexOf("=");
    if (index < 0) continue;
    if (part.slice(0, index).trim() !== name) continue;
    return decodeURIComponent(part.slice(index + 1).trim());
  }
  return undefined;
}

function sendJson(res: ServerResponse, status: number, value: unknown): void {
  const body = Buffer.from(JSON.stringify(value));
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": String(body.length),
    "x-content-type-options": "nosniff",
  });
  res.end(body);
}

function sendError(res: ServerResponse, error: unknown): void {
  const status =
    error instanceof HttpError || error instanceof RemoteError
      ? error.status
      : error instanceof Error && /not found/i.test(error.message)
        ? 404
        : 502;
  sendJson(res, status, { error: error instanceof Error ? error.message : String(error) });
}

function sendBytes(res: ServerResponse, status: number, kind: string, body: Buffer, extra: Record<string, string> = {}): void {
  res.writeHead(status, {
    "content-type": kind,
    "content-length": String(body.length),
    "x-content-type-options": "nosniff",
    ...extra,
  });
  res.end(body);
}

async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as string);
    size += buffer.length;
    if (size > JSON_LIMIT) throw fail(413, "Request body is too large");
    chunks.push(buffer);
  }
  if (size === 0) return {};
  let parsed: unknown;
  try {
    parsed = JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw fail(400, "Invalid JSON body");
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw fail(400, "Expected a JSON object");
  }
  return parsed as Record<string, unknown>;
}

function text(value: unknown, name: string, max = 2000): string {
  if (typeof value !== "string") throw fail(400, `${name} must be a string`);
  if (value.length > max) throw fail(400, `${name} is too long`);
  return value;
}

function optionalText(value: unknown, name: string, max = 2000): string | undefined {
  return value === undefined ? undefined : text(value, name, max);
}

function integer(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isInteger(value)) throw fail(400, `${name} must be an integer`);
  return value;
}

function boolean(value: unknown, name: string): boolean {
  if (typeof value !== "boolean") throw fail(400, `${name} must be a boolean`);
  return value;
}

function companionId(value: string): string {
  if (!/^[a-z0-9][a-z0-9_-]{0,63}$/i.test(value)) throw fail(400, "Invalid companion ID");
  return value.toLowerCase();
}

function slug(name: string): string {
  const base = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 48);
  return base === "" ? `companion-${Date.now().toString(36)}` : base;
}

function companionEntry(db: LarkDb, router: Router, id: string): Record<string, unknown> {
  const companion = db.getCompanion(id);
  if (companion === undefined) throw fail(404, "Companion not found");
  const online = router.onlineTerminals(id).length > 0;
  return {
    id: companion.id,
    name: companion.name,
    form: companion.form,
    personality: companion.personality,
    voice: companion.voice === "" ? "af_heart" : companion.voice,
    listening: companion.listening,
    portrait_id: companion.portraitId,
    dc_account_id: companion.dcAccountId,
    online,
    dormant: !online && !companion.configured,
    live_state: router.liveState(id),
  };
}

function terminalEntry(db: LarkDb, router: Router, id: string): Record<string, unknown> {
  const row = db.getTerminal(id);
  const live = router.onlineTerminals().find((state) => state.id === id);
  return {
    id,
    companion_id: live?.companionId ?? row?.companionId ?? "",
    kind: row?.kind ?? "terminal",
    online: live !== undefined,
    ip: live?.info.ip ?? row?.ip ?? "",
    firmware: live?.info.firmware ?? "",
    board: live?.info.board ?? row?.kind ?? "",
    volume: db.getTerminalVolume(id),
    ...(live?.telemetry === undefined
      ? {}
      : { battery_pct: live.telemetry.battery_pct, charging: live.telemetry.charging }),
  };
}

function remotePage(name: string, token: string): string {
  const dir = DEFAULT_REMOTE_DIR;
  let index = "";
  try {
    index = readFileSync(join(dir, "index.html"), "utf8");
  } catch {
    throw fail(500, "Remote page unavailable");
  }
  return index
    .replaceAll("{{TOKEN}}", encodeURIComponent(token))
    .replaceAll("{{TOKEN_HTML}}", token.replaceAll("&", "&amp;").replaceAll('"', "&#34;").replaceAll("<", "&lt;"))
    .replaceAll("{{NAME}}", name.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;"));
}

function authToken(req: IncomingMessage, query: URL): string {
  const header = req.headers["x-lark-remote-token"];
  if (typeof header === "string" && header !== "") return header;
  const fromQuery = query.searchParams.get("token");
  if (fromQuery !== null && fromQuery !== "") return fromQuery;
  return cookieValue(req, REMOTE_COOKIE) ?? "";
}

function cursorFrom(raw: string): { beforeTs: number; beforeId: string } | undefined {
  if (raw === "") return undefined;
  let decoded: string;
  try {
    decoded = Buffer.from(raw, "base64url").toString("utf8");
  } catch {
    throw fail(400, "Invalid history cursor");
  }
  const index = decoded.indexOf(":");
  if (index <= 0) throw fail(400, "Invalid history cursor");
  const beforeTs = Number.parseInt(decoded.slice(0, index), 10);
  const beforeId = decoded.slice(index + 1);
  if (!Number.isInteger(beforeTs) || beforeId === "") throw fail(400, "Invalid history cursor");
  return { beforeTs, beforeId };
}

export async function startHttp(options: HttpOptions): Promise<RunningHttp> {
  const { db, router, remote, captures } = options;
  const remoteDir = options.remoteDir ?? DEFAULT_REMOTE_DIR;
  const libraryDir = options.libraryDir ?? join(options.dataDir, "library");
  const server = createServer((req, res) => {
    void handle(req, res).catch((error: unknown) => {
      if (!res.headersSent) sendError(res, error);
      else res.end();
    });
  });

  function publicAsset(path: string): boolean {
    return path === `${REMOTE_PATH}/page.css` || path === `${REMOTE_PATH}/app.js`;
  }

  async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
    const url = new URL(req.url ?? "/", `http://${req.headers.host ?? "127.0.0.1"}`);
    const path = url.pathname;
    res.setHeader("cache-control", "no-store");

    if (publicAsset(path)) {
      const name = path.slice(`${REMOTE_PATH}/`.length);
      try {
        const body = readFileSync(join(remoteDir, name));
        const kind = extname(name) === ".css" ? "text/css; charset=utf-8" : "text/javascript; charset=utf-8";
        sendBytes(res, 200, kind, body, { "content-security-policy": REMOTE_CSP });
      } catch {
        throw fail(404, "Not found");
      }
      return;
    }

    if (path === `${REMOTE_PATH}` || path.startsWith(`${REMOTE_PATH}/`)) {
      await handleRemote(req, res, url);
      return;
    }

    if (path.startsWith("/api/") || path.startsWith("/ota/")) {
      if (!tokenMatches(req.headers["x-lark-token"], options.adminToken)) throw fail(401, "Unauthorized");
      if (path.startsWith("/ota/")) {
        await handleOta(req, res, path);
        return;
      }
      await handleAdmin(req, res, path);
      return;
    }

    if (path === "/health" && req.method === "GET") {
      const models: Record<string, { state: string }> = {};
      for (const [name, state] of router.models) models[name] = { state };
      if (Object.keys(models).length === 0) {
        try {
          const health = await options.edge.health();
          for (const [name, state] of Object.entries(health.models)) models[name] = { state: state.state, ...(state.error === undefined ? {} : { error: state.error }) };
        } catch {
          // Edge restarting; report models as unavailable below.
        }
      }
      const ready = Object.values(models).every((model) => model.state === "ready");
      sendJson(res, 200, {
        ok: true,
        stt: models.parakeet?.state === "ready",
        tts: models.kokoro?.state === "ready",
        opencode: options.opencode ?? true,
        deltachat: options.dc?.ready ?? false,
        smart_turn: models.smart_turn?.state === "ready",
        models,
        models_ready: ready,
      });
      return;
    }

    throw fail(404, "Not found");
  }

  async function handleAdmin(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
    const method = req.method ?? "GET";
    if (path === "/api/diagnostics" && method === "GET") {
      sendJson(res, 200, {
        ...router.diagnostics(),
        deltachat: options.dc === undefined ? { ready: false } : await options.dc.reconcile().catch(() => ({ ready: false })),
      });
      return;
    }
    if (path === "/api/library" && method === "GET") {
      sendJson(res, 200, planLibrary(libraryDir));
      return;
    }
    if (path === "/api/companions") {
      if (method === "GET") {
        sendJson(res, 200, { companions: db.listCompanions().map((companion) => companionEntry(db, router, companion.id)) });
        return;
      }
      if (method === "POST") {
        const body = await readJson(req);
        const name = text(body.name, "name", 80).trim();
        if (name === "") throw fail(400, "name cannot be empty");
        const form = optionalText(body.form, "form", 80) ?? "";
        const id = slug(name);
        if (db.getCompanion(id) !== undefined) throw fail(409, "A companion with that name exists");
        db.upsertCompanion({ id, name, form, configured: true, listening: false, voice: "af_heart" });
        void options.dc?.syncAccounts().catch(() => undefined);
        sendJson(res, 201, companionEntry(db, router, id));
        return;
      }
      throw fail(404, "Not found");
    }
    if (path.startsWith("/api/companions/")) {
      const id = companionId(path.slice("/api/companions/".length).split("/")[0] ?? "");
      if (db.getCompanion(id) === undefined) throw fail(404, "Companion not found");
      if (method === "GET") {
        sendJson(res, 200, companionEntry(db, router, id));
        return;
      }
      if (method === "PATCH" || method === "PUT") {
        const body = await readJson(req);
        const patch: Parameters<LarkDb["patchCompanion"]>[1] = {};
        if (body.name !== undefined) patch.name = text(body.name, "name", 80).trim() || "Lark";
        if (body.personality !== undefined) patch.personality = text(body.personality, "personality", 2000);
        if (body.voice !== undefined) patch.voice = text(body.voice, "voice", 64);
        if (body.form !== undefined) patch.form = text(body.form, "form", 80);
        if (body.listening !== undefined) patch.listening = boolean(body.listening, "listening");
        if (body.configured !== undefined) patch.configured = boolean(body.configured, "configured");
        if (body.portrait_id !== undefined) patch.portraitId = text(body.portrait_id, "portrait_id", 128);
        const saved = db.patchCompanion(id, patch);
        if (saved === undefined) throw fail(404, "Companion not found");
        if (patch.listening !== undefined) await router.setListening(id, patch.listening);
        if (patch.voice !== undefined) await router.setVoice(id, patch.voice, false);
        await router.refreshCompanion(id);
        if (body.portrait !== undefined) {
          const spec = portraitSpec(body.portrait);
          if (options.portraits === undefined) throw fail(503, "Portrait generation unavailable");
          void options.portraits.request(id, spec).catch(() => undefined);
        }
        sendJson(res, 200, companionEntry(db, router, id));
        return;
      }
      if (method === "DELETE") {
        await options.dc?.removeCompanion(id);
        db.deleteCompanion(id);
        res.writeHead(204).end();
        return;
      }
      throw fail(404, "Not found");
    }
    if (path === "/api/terminals" && method === "GET") {
      sendJson(res, 200, { terminals: db.listTerminals().map((row) => terminalEntry(db, router, row.id)) });
      return;
    }
    if (path.startsWith("/api/terminals/")) {
      const rest = path.slice("/api/terminals/".length);
      if (rest.endsWith("/hits") && method === "GET") {
        const id = rest.slice(0, -"/hits".length);
        try {
          const reply = await options.edge.hits(id);
          sendJson(res, 200, { ...reply, hits: reply.hits.map((hit: EdgeHit) => ({ id: hit.ID, label: hit.Label ?? "", action: "", value: "", x1: hit.X1, y1: hit.Y1, x2: hit.X2, y2: hit.Y2 })) });
        } catch (error) {
          throw fail(404, error instanceof Error ? error.message : "Terminal not connected");
        }
        return;
      }
      if (rest.endsWith("/attach") && method === "POST") {
        const id = rest.slice(0, -"/attach".length);
        const body = await readJson(req);
        const companion = companionId(text(body.companion_id, "companion_id", 64));
        if (db.getCompanion(companion) === undefined) throw fail(404, "Companion not found");
        const row = db.getTerminal(id);
        db.upsertTerminal({ id, kind: row?.kind ?? "terminal", companionId: companion, ip: row?.ip ?? "", port: row?.port ?? 0, lastSeen: row?.lastSeen ?? Date.now(), createdAt: row?.createdAt ?? new Date().toISOString() });
        sendJson(res, 200, terminalEntry(db, router, id));
        return;
      }
      if (rest.endsWith("/detach") && method === "POST") {
        const id = rest.slice(0, -"/detach".length);
        const row = db.getTerminal(id);
        if (row === undefined) throw fail(404, "Terminal not found");
        db.upsertTerminal({ ...row, companionId: null });
        sendJson(res, 200, terminalEntry(db, router, id));
        return;
      }
      if (rest.endsWith("/volume") && method === "POST") {
        const id = rest.slice(0, -"/volume".length);
        const body = await readJson(req);
        router.setVolume(id, integer(body.volume, "volume"));
        sendJson(res, 200, terminalEntry(db, router, id));
        return;
      }
      const id = rest;
      if (method === "GET") {
        sendJson(res, 200, terminalEntry(db, router, id));
        return;
      }
      if (method === "PATCH" || method === "PUT") {
        const body = await readJson(req);
        const row = db.getTerminal(id);
        if (body.volume !== undefined) router.setVolume(id, integer(body.volume, "volume"));
        if (row === undefined && body.companion_id === undefined) throw fail(404, "Terminal not found");
        if (body.companion_id !== undefined) {
          const companion = companionId(text(body.companion_id, "companion_id", 64));
          db.upsertTerminal({ id, kind: row?.kind ?? "terminal", companionId: companion, ip: row?.ip ?? "", port: row?.port ?? 0, lastSeen: row?.lastSeen ?? Date.now(), createdAt: row?.createdAt ?? new Date().toISOString() });
        }
        sendJson(res, 200, terminalEntry(db, router, id));
        return;
      }
      if (method === "DELETE") {
        db.deleteTerminal(id);
        res.writeHead(204).end();
        return;
      }
      throw fail(404, "Not found");
    }
    if (path === "/api/capture" && method === "POST") {
      const body = await readJson(req);
      const companion = companionId(text(body.companion_id, "companion_id", 64));
      const seconds = body.seconds === undefined ? CAPTURE_DEFAULT_SECONDS : integer(body.seconds, "seconds");
      if (["listening", "thinking", "speaking"].includes(router.liveState(companion))) {
        throw fail(409, "The companion is busy; try again when the turn ends");
      }
      const started = await captures.start(companion, seconds);
      const result = await captures.wait(companion, (seconds + 30) * 1000);
      if (result.error !== undefined) throw fail(502, result.error);
      sendJson(res, 200, { path: result.path, companion_id: companion, seconds: result.seconds, name: started.name });
      return;
    }
    if (path === "/api/remote/tunnels" && method === "GET") {
      sendJson(res, 200, { tunnels: remote.listTunnels() });
      return;
    }
    if (path.startsWith("/api/remote/tunnels/") && method === "DELETE") {
      const id = companionId(path.slice("/api/remote/tunnels/".length));
      await remote.stopCompanion(id);
      res.writeHead(204).end();
      return;
    }
    throw fail(404, "Not found");
  }

  async function handleRemote(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
    const path = url.pathname;
    res.setHeader("referrer-policy", "no-referrer");
    const raw = authToken(req, url);
    const scope = remote.authorize(raw);
    if (scope === undefined) throw fail(401, "Unauthorized");

    if (path === REMOTE_PATH && req.method === "GET") {
      const companion = db.getCompanion(scope.companionId);
      const name = companion?.name ?? "Lark";
      const body = Buffer.from(remotePage(name, raw));
      res.writeHead(200, {
        "content-type": "text/html; charset=utf-8",
        "content-length": String(body.length),
        "content-security-policy": REMOTE_CSP,
        "x-content-type-options": "nosniff",
        "set-cookie": `${REMOTE_COOKIE}=${encodeURIComponent(raw)}; Path=${REMOTE_PATH}; HttpOnly; Secure; SameSite=Lax`,
      });
      res.end(body);
      return;
    }

    if (path === `${REMOTE_PATH}/events` && req.method === "GET") {
      await streamEvents(req, res, scope);
      return;
    }

    if (path.startsWith(`${REMOTE_PATH}/pwa/`)) {
      await servePwa(req, res, path, scope, raw);
      return;
    }

    if (path.startsWith(`${REMOTE_PATH}/api/`)) {
      await handleRemoteApi(req, res, path, scope);
      return;
    }
    throw fail(404, "Not found");
  }

  async function servePwa(_req: IncomingMessage, res: ServerResponse, path: string, scope: RemoteTokenScope, raw: string): Promise<void> {
    const companion = db.getCompanion(scope.companionId);
    if (path === `${REMOTE_PATH}/pwa/manifest.webmanifest`) {
      const manifest = {
        name: companion?.name ?? "Lark",
        short_name: companion?.name ?? "Lark",
        start_url: `${REMOTE_PATH}?token=${encodeURIComponent(raw)}`,
        scope: REMOTE_PATH,
        display: "standalone",
        background_color: "#6f5bd3",
        theme_color: "#6f5bd3",
        icons: [
          { src: `${REMOTE_PATH}/pwa/icon-192.png`, sizes: "192x192", type: "image/png", purpose: "any" },
          { src: `${REMOTE_PATH}/pwa/icon-512.png`, sizes: "512x512", type: "image/png", purpose: "any" },
        ],
      };
      sendBytes(res, 200, "application/manifest+json", Buffer.from(JSON.stringify(manifest)), {
        "content-security-policy": REMOTE_CSP,
      });
      return;
    }
    if (path === `${REMOTE_PATH}/pwa/icon-192.png` || path === `${REMOTE_PATH}/pwa/icon-512.png` || path === `${REMOTE_PATH}/pwa/maskable-512.png`) {
      let body = ONE_PIXEL_PNG;
      const portraitDir = options.portraits?.dirFor(companion?.portraitId ?? "");
      if (portraitDir !== undefined) {
        try {
          body = readFileSync(join(portraitDir, "sheet.png"));
        } catch {
          body = ONE_PIXEL_PNG;
        }
      }
      sendBytes(res, 200, "image/png", body, { "content-security-policy": REMOTE_CSP });
      return;
    }
    throw fail(404, "Not found");
  }

  async function streamEvents(req: IncomingMessage, res: ServerResponse, scope: RemoteTokenScope): Promise<void> {
    res.writeHead(200, {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache",
      connection: "keep-alive",
      "x-accel-buffering": "no",
    });
    res.write("retry: 2000\n\n");
    let closed = false;
    req.on("close", () => {
      closed = true;
    });
    const initial = router.snapshot(scope.companionId);
    const send = (event: string, value: unknown): boolean => {
      if (closed) return false;
      res.write(`event: ${event}\ndata: ${JSON.stringify(value)}\n\n`);
      return true;
    };
    if (!send("state", initial)) return;
    let key = snapshotKey(initial);
    let captureKey = JSON.stringify(captures.status(scope.companionId));
    if (!send("capture", captures.status(scope.companionId))) return;
    const tick = setInterval(() => {
      if (closed) return;
      const snapshot = router.snapshot(scope.companionId);
      const nextKey = snapshotKey(snapshot);
      if (nextKey !== key) {
        key = nextKey;
        const update: Record<string, unknown> = { ...snapshot };
        if (update.companion !== null && typeof update.companion === "object") {
          update.companion = { ...(update.companion as Record<string, unknown>) };
          delete (update.companion as Record<string, unknown>).portrait;
        }
        if (!send("state", update)) return;
      }
      const status = captures.status(scope.companionId);
      const nextCaptureKey = JSON.stringify(status);
      if (nextCaptureKey !== captureKey) {
        captureKey = nextCaptureKey;
        send("capture", status);
      }
    }, REMOTE_POLL_MS);
    const keep = setInterval(() => {
      if (closed) return;
      res.write(": keep-alive\n\n");
    }, REMOTE_KEEPALIVE_MS);
    req.on("close", () => {
      clearInterval(tick);
      clearInterval(keep);
      res.end();
    });
  }

  async function handleRemoteApi(req: IncomingMessage, res: ServerResponse, path: string, scope: RemoteTokenScope): Promise<void> {
    const method = req.method ?? "GET";
    const companionId = scope.companionId;
    const api = `${REMOTE_PATH}/api/`;
    const name = path.slice(api.length);
    if (name === "say" && method === "POST") {
      const body = await readJson(req);
      const message = text(body.text, "text", 500).trim();
      if (message === "") throw fail(400, "Enter something to say");
      await router.sayNow(companionId, message);
      sendJson(res, 200, { ok: true, message: "Said it.", state: router.snapshot(companionId) });
      return;
    }
    if (name === "listen" && method === "POST") {
      const body = await readJson(req);
      await router.setListening(companionId, boolean(body.listening, "listening"));
      sendJson(res, 200, { ok: true, message: "Listening mode updated.", state: router.snapshot(companionId) });
      return;
    }
    if (name === "volume" && method === "POST") {
      const body = await readJson(req);
      const terminal = text(body.terminal, "terminal", 128).trim();
      if (terminal === "") throw fail(400, "Terminal is required");
      const owned = router.onlineTerminals(companionId).some((state) => state.id === terminal) || db.getTerminal(terminal)?.companionId === companionId;
      if (!owned) throw fail(403, "Terminal out of scope");
      router.setVolume(terminal, integer(body.volume, "volume"));
      sendJson(res, 200, { ok: true, message: "Volume updated.", state: router.snapshot(companionId) });
      return;
    }
    if (name === "voice" && method === "POST") {
      const body = await readJson(req);
      await router.setVoice(companionId, text(body.voice, "voice", 64), true);
      sendJson(res, 200, { ok: true, message: "Voice updated.", state: router.snapshot(companionId) });
      return;
    }
    if (name === "story/open" || name === "story/restart") {
      if (method !== "POST") throw fail(404, "Not found");
      const body = await readJson(req);
      const story = text(body.story, "story", 64).trim();
      if (story === "") throw fail(400, "Missing story ID");
      if (name.endsWith("restart")) await router.restartStory(companionId, story);
      else await router.openStory(companionId, story, false);
      sendJson(res, 200, { ok: true, message: name.endsWith("restart") ? "Started over on device." : "Opened on device.", state: router.snapshot(companionId) });
      return;
    }
    if (name === "history" && method === "GET") {
      const url = new URL(req.url ?? "/", "http://127.0.0.1");
      const channelRaw = url.searchParams.get("channel") ?? "all";
      if (!["all", "in_person", "deltachat"].includes(channelRaw)) throw fail(400, "Unknown history filter");
      const limit = Math.max(1, Math.min(50, Number.parseInt(url.searchParams.get("limit") ?? "20", 10) || 20));
      const q = (url.searchParams.get("q") ?? "").slice(0, 100);
      const cursor = cursorFrom(url.searchParams.get("cursor") ?? "");
      sendJson(res, 200, router.history(companionId, channelRaw as "all" | "in_person" | "deltachat", scope.contact, scope.contactName, q, limit, cursor));
      return;
    }
    if (name === "capture" && method === "POST") {
      const body = await readJson(req);
      const seconds = body.seconds === undefined ? CAPTURE_PAGE_SECONDS : integer(body.seconds, "seconds");
      if (["listening", "thinking", "speaking"].includes(router.liveState(companionId))) {
        throw fail(409, "The companion is busy; try again when the turn ends");
      }
      const started = await captures.start(companionId, seconds);
      sendJson(res, 200, started);
      return;
    }
    if (name === "captures" && method === "GET") {
      sendJson(res, 200, { captures: captures.list(companionId) });
      return;
    }
    if (name.startsWith("captures/")) {
      const rest = name.slice("captures/".length);
      if (rest.endsWith("/transcribe") && method === "POST") {
        const capture = decodeURIComponent(rest.slice(0, -"/transcribe".length));
        sendJson(res, 200, await captures.transcribe(companionId, capture));
        return;
      }
      if (rest.endsWith(".wav") && method === "GET") {
        serveCaptureWav(req, res, companionId, decodeURIComponent(rest.slice(0, -4)));
        return;
      }
      if (method === "DELETE") {
        captures.remove(companionId, decodeURIComponent(rest));
        res.writeHead(204).end();
        return;
      }
    }
    if (name === "revoke" && method === "POST") {
      await remote.revokeContact(companionId, scope.contact);
      res.setHeader("set-cookie", `${REMOTE_COOKIE}=; Path=${REMOTE_PATH}; Max-Age=0; HttpOnly; Secure; SameSite=Lax`);
      sendJson(res, 200, { ok: true, message: "Remote controls off." });
      return;
    }
    throw fail(404, "Not found");
  }

  function serveCaptureWav(req: IncomingMessage, res: ServerResponse, companionId: string, capture: string): void {
    let path: string;
    try {
      path = captures.resolvePath(companionId, capture);
    } catch (error) {
      sendError(res, error);
      return;
    }
    const info = statSync(path);
    const range = req.headers.range;
    if (range === undefined) {
      const body = readFileSync(path);
      sendBytes(res, 200, "audio/wav", body, { "accept-ranges": "bytes" });
      return;
    }
    const match = /^bytes=(\d*)-(\d*)$/.exec(range.trim());
    if (match === null) {
      res.writeHead(416, { "content-range": `bytes */${info.size}` }).end();
      return;
    }
    const start = match[1] === "" ? 0 : Number.parseInt(match[1] ?? "0", 10);
    const end = match[2] === "" ? info.size - 1 : Math.min(info.size - 1, Number.parseInt(match[2] ?? "", 10));
    if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || start > end) {
      res.writeHead(416, { "content-range": `bytes */${info.size}` }).end();
      return;
    }
    const body = readFileSync(path).subarray(start, end + 1);
    res.writeHead(206, {
      "content-type": "audio/wav",
      "content-length": String(body.length),
      "content-range": `bytes ${start}-${end}/${info.size}`,
      "accept-ranges": "bytes",
      "x-content-type-options": "nosniff",
    });
    res.end(body);
  }

  async function handleOta(req: IncomingMessage, res: ServerResponse, path: string): Promise<void> {
    if (req.method !== "GET") throw fail(404, "Not found");
    const root = join(options.dataDir, "ota");
    const relative = normalize(path.slice("/ota/".length));
    if (relative.startsWith("..") || relative.includes("\0")) throw fail(404, "Not found");
    const file = join(root, relative);
    if (!file.startsWith(root) || !existsSync(file)) throw fail(404, "Not found");
    const body = readFileSync(file);
    const kind = extname(file) === ".json" ? "application/json; charset=utf-8" : "application/octet-stream";
    sendBytes(res, 200, kind, body);
  }

  await new Promise<void>((resolveListen, rejectListen) => {
    const onError = (error: Error): void => rejectListen(error);
    server.once("error", onError);
    server.listen(options.port, options.host, () => {
      server.off("error", onError);
      resolveListen();
    });
  });
  const address = server.address();
  const port = address !== null && typeof address === "object" ? address.port : options.port;

  return {
    server,
    port,
    async close() {
      // SSE clients never finish on their own; stop them before waiting for
      // the listener so a signal cannot hang behind an open remote page.
      server.closeAllConnections();
      await new Promise<void>((resolveClose) => server.close(() => resolveClose()));
    },
  };
}

function snapshotKey(snapshot: ReturnType<Router["snapshot"]>): string {
  const copy = structuredClone(snapshot) as unknown as Record<string, unknown>;
  if (copy.companion !== null && typeof copy.companion === "object") {
    delete (copy.companion as Record<string, unknown>).portrait;
  }
  return JSON.stringify(copy);
}

function portraitSpec(raw: unknown): PortraitSpec {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw fail(400, "portrait must be an object");
  }
  const value = raw as Record<string, unknown>;
  const prompt = text(value.prompt, "portrait.prompt", 600).trim();
  if (prompt === "") throw fail(400, "portrait.prompt is required");
  const spec: PortraitSpec = { prompt };
  if (value.form !== undefined) spec.form = text(value.form, "portrait.form", 160);
  if (value.recolor !== undefined) spec.recolor = text(value.recolor, "portrait.recolor", 200);
  if (value.edit !== undefined) spec.edit = boolean(value.edit, "portrait.edit");
  if (value.style === "pixel") spec.style = "pixel";
  return spec;
}
