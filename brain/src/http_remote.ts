// Scoped /remote page: token auth, SSE state/capture streams, remote commands,
// the PWA manifest and icons, history, and byte-range WAV playback.
import { readFileSync, statSync } from "node:fs";
import { open, type FileHandle } from "node:fs/promises";
import type { IncomingMessage, ServerResponse } from "node:http";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import type { LarkDb } from "./db.ts";
import type { RemoteSnapshot, Router } from "./router.ts";
import {
  CAPTURE_PAGE_SECONDS,
  REMOTE_COOKIE,
  REMOTE_PATH,
  type CaptureStore,
  type RemoteManager,
  type RemoteTokenScope,
} from "./remote.ts";
import type { PortraitManager } from "./portraits.ts";
import {
  REMOTE_CSP,
  assertIdle,
  boolean,
  cookieValue,
  fail,
  integer,
  okState,
  readJson,
  requiredText,
  sendBytes,
  sendError,
  sendJson,
  text,
} from "./http_common.ts";

// Burst mutations (a turn updates listening, busy, messages and speech) share
// one SSE frame; 30 ms is below the page's perception threshold.
const REMOTE_PUSH_COALESCE_MS = 30;
const REMOTE_KEEPALIVE_MS = 15_000;

const HERE = fileURLToPath(new URL(".", import.meta.url));
export const DEFAULT_REMOTE_DIR = resolve(HERE, "../../web/remote");

const ONE_PIXEL_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
  "base64",
);

// The page template is static; cache it until its mtime or size changes so a
// reload only stats the file.
let indexTemplate: { mtimeMs: number; size: number; text: string } | undefined;

function remotePage(name: string, token: string): string {
  const file = join(DEFAULT_REMOTE_DIR, "index.html");
  let index = "";
  try {
    const info = statSync(file);
    if (indexTemplate === undefined || indexTemplate.mtimeMs !== info.mtimeMs || indexTemplate.size !== info.size) {
      indexTemplate = { mtimeMs: info.mtimeMs, size: info.size, text: readFileSync(file, "utf8") };
    }
    index = indexTemplate.text;
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

function snapshotKey(snapshot: RemoteSnapshot): string {
  return JSON.stringify(snapshot);
}

export interface RemoteRoutesOptions {
  db: LarkDb;
  router: Router;
  remote: RemoteManager;
  captures: CaptureStore;
  portraits?: PortraitManager;
  remoteDir?: string;
}

/** Build the /remote request handler around the shared admin state. */
export function createRemoteRoutes(options: RemoteRoutesOptions): {
  handle(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void>;
} {
  const { db, router, remote, captures } = options;
  const portraits = options.portraits;
  // The portrait sheet is at most one image per companion generation; keep the
  // last body so repeated page loads and SSE reconnects avoid disk reads.
  const portraitCache = new Map<string, { etag: string; body: Buffer }>();

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
      const portraitDir = portraits?.dirFor(companion?.portraitId ?? "");
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
    const initialCapture = captures.status(scope.companionId);
    let captureKey = JSON.stringify(initialCapture);
    if (!send("capture", initialCapture)) return;

    let timer: ReturnType<typeof setTimeout> | undefined;
    const flush = (): void => {
      timer = undefined;
      if (closed) return;
      const snapshot = router.snapshot(scope.companionId);
      const nextKey = snapshotKey(snapshot);
      if (nextKey !== key) {
        key = nextKey;
        if (!send("state", snapshot)) return;
      }
      const status = captures.status(scope.companionId);
      const nextCaptureKey = JSON.stringify(status);
      if (nextCaptureKey !== captureKey) {
        captureKey = nextCaptureKey;
        send("capture", status);
      }
    };
    const schedule = (): void => {
      if (closed || timer !== undefined) return;
      timer = setTimeout(flush, REMOTE_PUSH_COALESCE_MS);
      timer.unref?.();
    };
    const unsubscribeRouter = router.subscribe(schedule);
    const unsubscribeCaptures = captures.subscribe?.(schedule) ?? (() => undefined);
    // A slow safety net also refreshes capture countdowns, which tick without a
    // database mutation; real state changes never wait for it.
    const keep = setInterval(() => {
      if (closed) return;
      res.write(": keep-alive\n\n");
      flush();
    }, REMOTE_KEEPALIVE_MS);
    keep.unref?.();
    req.on("close", () => {
      if (timer !== undefined) clearTimeout(timer);
      clearInterval(keep);
      unsubscribeRouter();
      unsubscribeCaptures();
      res.end();
    });
  }

  function servePortrait(req: IncomingMessage, res: ServerResponse, companionId: string): void {
    const companion = db.getCompanion(companionId);
    const dir = portraits?.dirFor(companion?.portraitId ?? "");
    if (dir === undefined) throw fail(404, "No portrait");
    const file = join(dir, "sheet.png");
    let info;
    try {
      info = statSync(file);
    } catch {
      throw fail(404, "No portrait");
    }
    const etag = `"${Math.floor(info.mtimeMs)}-${info.size}"`;
    res.setHeader("etag", etag);
    res.setHeader("cache-control", "private, max-age=60");
    if (req.headers["if-none-match"] === etag) {
      res.writeHead(304).end();
      return;
    }
    let cached = portraitCache.get(file);
    if (cached === undefined || cached.etag !== etag) {
      let body: Buffer;
      try {
        body = readFileSync(file);
      } catch {
        throw fail(404, "No portrait");
      }
      portraitCache.clear();
      cached = { etag, body };
      portraitCache.set(file, cached);
    }
    sendBytes(res, 200, "image/png", cached.body, { etag, "cache-control": "private, max-age=60" });
  }

  async function handleRemoteApi(req: IncomingMessage, res: ServerResponse, path: string, scope: RemoteTokenScope): Promise<void> {
    const method = req.method ?? "GET";
    const companionId = scope.companionId;
    const api = `${REMOTE_PATH}/api/`;
    const name = path.slice(api.length);
    if (name === "portrait" && method === "GET") {
      servePortrait(req, res, companionId);
      return;
    }
    if (name === "say" && method === "POST") {
      const body = await readJson(req);
      const message = requiredText(body.text, "text", 500, "Enter something to say");
      await router.sayNow(companionId, message);
      okState(res, router, companionId, "Said it.");
      return;
    }
    if (name === "listen" && method === "POST") {
      const body = await readJson(req);
      await router.setListening(companionId, boolean(body.listening, "listening"));
      okState(res, router, companionId, "Listening mode updated.");
      return;
    }
    if (name === "volume" && method === "POST") {
      const body = await readJson(req);
      const terminal = requiredText(body.terminal, "terminal", 128, "Terminal is required");
      const owned = router.onlineTerminals(companionId).some((state) => state.id === terminal) || db.getTerminal(terminal)?.companionId === companionId;
      if (!owned) throw fail(403, "Terminal out of scope");
      router.setVolume(terminal, integer(body.volume, "volume"));
      okState(res, router, companionId, "Volume updated.");
      return;
    }
    if (name === "voice" && method === "POST") {
      const body = await readJson(req);
      await router.setVoice(companionId, text(body.voice, "voice", 64), true);
      okState(res, router, companionId, "Voice updated.");
      return;
    }
    if (name === "story/open" || name === "story/restart") {
      if (method !== "POST") throw fail(404, "Not found");
      const body = await readJson(req);
      const story = requiredText(body.story, "story", 64, "Missing story ID");
      if (name.endsWith("restart")) await router.restartStory(companionId, story);
      else await router.openStory(companionId, story, false);
      okState(res, router, companionId, name.endsWith("restart") ? "Started over on device." : "Opened on device.");
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
      assertIdle(router, companionId);
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
        await serveCaptureWav(req, res, companionId, decodeURIComponent(rest.slice(0, -4)));
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

  async function serveCaptureWav(req: IncomingMessage, res: ServerResponse, companionId: string, capture: string): Promise<void> {
    let path: string;
    try {
      path = captures.resolvePath(companionId, capture);
    } catch (error) {
      sendError(res, error);
      return;
    }
    let info;
    try {
      info = statSync(path);
    } catch (error) {
      sendError(res, error);
      return;
    }
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
    // Read only the requested slice; capture WAVs can be tens of MB and media
    // players seek through them constantly.
    const length = end - start + 1;
    const body = Buffer.allocUnsafe(length);
    let handle: FileHandle | undefined;
    try {
      handle = await open(path, "r");
      let offset = 0;
      while (offset < length) {
        const { bytesRead } = await handle.read(body, offset, length - offset, start + offset);
        if (bytesRead === 0) break;
        offset += bytesRead;
      }
      if (offset !== length) throw fail(500, "Short read from capture");
    } finally {
      await handle?.close().catch(() => undefined);
    }
    res.writeHead(206, {
      "content-type": "audio/wav",
      "content-length": String(body.length),
      "content-range": `bytes ${start}-${end}/${info.size}`,
      "accept-ranges": "bytes",
      "x-content-type-options": "nosniff",
    });
    res.end(body);
  }

  return { handle: handleRemote };
}
