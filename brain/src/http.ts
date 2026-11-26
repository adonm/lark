// One node:http server for the admin API, OTA files, device-facing diagnostics
// and the scoped /remote page. No web framework: explicit method/path switches,
// strict JSON decoding, timing-safe admin auth and one SSE stream per remote
// page keep the surface small and auditable.
import { existsSync, readFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { extname, join, normalize } from "node:path";
import type { LarkDb } from "./db.ts";
import type { EdgeClient, EdgeHit } from "./edge_client.ts";
import type { Router } from "./router.ts";
import { planLibrary } from "./library.ts";
import { errMessage, slugify } from "./util.ts";
import {
  CAPTURE_DEFAULT_SECONDS,
  REMOTE_PATH,
  type CaptureStore,
  type RemoteManager,
} from "./remote.ts";
import type { DcService } from "./deltachat.ts";
import type { PortraitManager, PortraitSpec } from "./portraits.ts";
import {
  REMOTE_CSP,
  assertIdle,
  boolean,
  fail,
  integer,
  optionalText,
  readJson,
  sendBytes,
  sendError,
  sendJson,
  text,
  tokenMatches,
} from "./http_common.ts";
import { DEFAULT_REMOTE_DIR, createRemoteRoutes } from "./http_remote.ts";

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

function companionId(value: string): string {
  if (!/^[a-z0-9][a-z0-9_-]{0,63}$/i.test(value)) throw fail(400, "Invalid companion ID");
  return value.toLowerCase();
}

function slug(name: string): string {
  return slugify(name, { max: 48, fallback: `companion-${Date.now().toString(36)}` });
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

export async function startHttp(options: HttpOptions): Promise<RunningHttp> {
  const { db, router, remote, captures } = options;
  const remoteDir = options.remoteDir ?? DEFAULT_REMOTE_DIR;
  const libraryDir = options.libraryDir ?? join(options.dataDir, "library");
  const remoteRoutes = createRemoteRoutes({
    db,
    router,
    remote,
    captures,
    ...(options.portraits === undefined ? {} : { portraits: options.portraits }),
    ...(options.remoteDir === undefined ? {} : { remoteDir: options.remoteDir }),
  });
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
      await remoteRoutes.handle(req, res, url);
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
          throw fail(404, errMessage(error));
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
      assertIdle(router, companion);
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
