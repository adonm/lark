// Shared HTTP primitives for the admin and remote routes: strict JSON
// decoding, bounded text fields, and one JSON/bytes error shape.
import { createHash, timingSafeEqual } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { Router } from "./router.ts";
import { RemoteError } from "./remote.ts";
import { errMessage } from "./util.ts";

export const REMOTE_CSP =
  "default-src 'none'; script-src 'self'; style-src 'self' https://cdn.jsdelivr.net; img-src 'self' data:; media-src 'self'; connect-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

const JSON_LIMIT = 1 << 20;

export class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "HttpError";
    this.status = status;
  }
}

export function fail(status: number, message: string): HttpError {
  return new HttpError(status, message);
}

function sha256(value: string): Buffer {
  return createHash("sha256").update(value).digest();
}

export function tokenMatches(header: string | string[] | undefined, expected: string): boolean {
  if (expected === "" || typeof header !== "string") return false;
  const left = sha256(header);
  const right = sha256(expected);
  return left.length === right.length && timingSafeEqual(left, right);
}

export function cookieValue(req: IncomingMessage, name: string): string | undefined {
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

export function sendJson(res: ServerResponse, status: number, value: unknown): void {
  const body = Buffer.from(JSON.stringify(value));
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": String(body.length),
    "x-content-type-options": "nosniff",
  });
  res.end(body);
}

export function sendError(res: ServerResponse, error: unknown): void {
  const status =
    error instanceof HttpError || error instanceof RemoteError
      ? error.status
      : error instanceof Error && /not found/i.test(error.message)
        ? 404
        : 502;
  sendJson(res, status, { error: errMessage(error) });
}

export function sendBytes(res: ServerResponse, status: number, kind: string, body: Buffer, extra: Record<string, string> = {}): void {
  res.writeHead(status, {
    "content-type": kind,
    "content-length": String(body.length),
    "x-content-type-options": "nosniff",
    ...extra,
  });
  res.end(body);
}

export interface CachedAsset {
  mtimeMs: number;
  size: number;
  etag: string;
  body: Buffer;
}

export interface CachedAssetOptions {
  file: string;
  cache: Map<string, CachedAsset>;
  kind: string;
  /** Extra response headers, always alongside the derived etag. */
  headers: Record<string, string>;
  /** 404 message when the file is missing or unreadable. */
  notFound?: string;
  /** Wrap read errors as 404s with this message instead of surfacing a 502. */
  readNotFound?: string;
  /** Portrait sheets are one file; drop the previous entry on a rewrite. */
  exclusive?: boolean;
}

/** Stat, revalidate and serve one disk-backed asset with a cheap ETag. */
export function serveCachedAsset(req: IncomingMessage, res: ServerResponse, options: CachedAssetOptions): void {
  let info;
  try {
    info = statSync(options.file);
  } catch {
    throw fail(404, options.notFound ?? "Not found");
  }
  const etag = `"${Math.floor(info.mtimeMs)}-${info.size}"`;
  let cached = options.cache.get(options.file);
  if (cached === undefined || cached.mtimeMs !== info.mtimeMs || cached.size !== info.size) {
    let body: Buffer;
    if (options.readNotFound === undefined) {
      body = readFileSync(options.file);
    } else {
      try {
        body = readFileSync(options.file);
      } catch {
        throw fail(404, options.readNotFound);
      }
    }
    if (options.exclusive === true) options.cache.clear();
    cached = { mtimeMs: info.mtimeMs, size: info.size, etag, body };
    options.cache.set(options.file, cached);
  }
  const headers = { etag, ...options.headers };
  if (req.headers["if-none-match"] === cached.etag) {
    res.writeHead(304, headers).end();
    return;
  }
  sendBytes(res, 200, options.kind, cached.body, headers);
}

export async function readJson(req: IncomingMessage): Promise<Record<string, unknown>> {
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

export function text(value: unknown, name: string, max = 2000): string {
  if (typeof value !== "string") throw fail(400, `${name} must be a string`);
  if (value.length > max) throw fail(400, `${name} is too long`);
  return value;
}

export function optionalText(value: unknown, name: string, max = 2000): string | undefined {
  return value === undefined ? undefined : text(value, name, max);
}

export function integer(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isInteger(value)) throw fail(400, `${name} must be an integer`);
  return value;
}

export function boolean(value: unknown, name: string): boolean {
  if (typeof value !== "boolean") throw fail(400, `${name} must be a boolean`);
  return value;
}

export function requiredText(value: unknown, name: string, max: number, emptyError: string): string {
  const trimmed = text(value, name, max).trim();
  if (trimmed === "") throw fail(400, emptyError);
  return trimmed;
}

export function assertIdle(router: Router, companionId: string): void {
  if (["listening", "thinking", "speaking"].includes(router.liveState(companionId))) {
    throw fail(409, "The companion is busy; try again when the turn ends");
  }
}

export function okState(res: ServerResponse, router: Router, companionId: string, message: string): void {
  sendJson(res, 200, { ok: true, message, state: router.snapshot(companionId) });
}
