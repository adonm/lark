// Local microphone captures: single-flight jobs, progress, retention pruning,
// transcripts, and path scoping by companion.
import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import type { EdgeCaptureDone, EdgeCaptureProgress } from "./edge_client.ts";
import { RemoteError } from "./remote.ts";
import { errMessage } from "./util.ts";

export const CAPTURE_RETENTION_MS = 14 * 24 * 60 * 60 * 1000;
export const CAPTURE_MAX_BYTES = 1 << 30;
export const CAPTURE_DONE_WINDOW_MS = 20_000;
export const CAPTURE_DEFAULT_SECONDS = 15;
export const CAPTURE_PAGE_SECONDS = 60;

// ---- captures ---------------------------------------------------------------

export interface CaptureEdge {
  captureStart(terminalId: string, outPath: string, seconds: number): Promise<void>;
  on(name: "capture.done", listener: (event: EdgeCaptureDone) => void): () => void;
  on(name: "capture.progress", listener: (event: EdgeCaptureProgress) => void): () => void;
  transcribe?(path: string): Promise<string>;
}

export interface CaptureItem {
  name: string;
  seconds: number;
  size: number;
  created: string;
  transcript?: string;
  computing?: boolean;
}

export interface CaptureStatus {
  state: "idle" | "recording" | "done";
  id?: string;
  name?: string;
  seconds?: number;
  remaining?: number;
  started_at?: string;
  ok?: boolean;
  error?: string;
}

interface CaptureJob {
  name: string;
  companionId: string;
  seconds: number;
  remaining: number;
  startedAt: number;
  terminalId: string;
  outPath: string;
  resolve: (result: { name: string; seconds: number; path: string; error?: string }) => void;
}

interface CaptureDone {
  name: string;
  ok: boolean;
  at: number;
  error?: string;
}

export interface CaptureStoreOptions {
  dir: string;
  edge: CaptureEdge;
  terminalForCompanion: (companionId: string) => string | undefined;
  companionForTerminal: (terminalId: string) => string | undefined;
  now?: () => number;
  log?: (line: string) => void;
  retentionMs?: number;
  maxBytes?: number;
}

const CAPTURE_NAME = /^capture-(.+)-(\d{8}-\d{6})(?:-(\d+))?\.wav$/;

export function captureOwner(name: string): string | undefined {
  const match = CAPTURE_NAME.exec(name);
  return match === null ? undefined : match[1];
}

export function captureId(name: string): string {
  return name.endsWith(".wav") ? name.slice(0, -4) : name;
}

export function pruneCaptures(dir: string, maxAgeMs = CAPTURE_RETENTION_MS, maxBytes = CAPTURE_MAX_BYTES): number {
  if (!existsSync(dir)) return 0;
  const groups = new Map<string, { base: string; paths: string[]; mtime: number; size: number }>();
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory() || entry.name.startsWith(".")) continue;
    const base = entry.name.endsWith(".wav") || entry.name.endsWith(".txt") ? entry.name.slice(0, -4) : entry.name;
    if (captureOwner(`${base}.wav`) === undefined) continue;
    const path = join(dir, entry.name);
    let info;
    try {
      info = statSync(path);
    } catch {
      continue;
    }
    const group = groups.get(base) ?? { base, paths: [], mtime: info.mtimeMs, size: 0 };
    group.paths.push(path);
    group.mtime = Math.max(group.mtime, info.mtimeMs);
    group.size += info.size;
    groups.set(base, group);
  }
  const now = Date.now();
  let total = 0;
  for (const group of groups.values()) total += group.size;
  let removed = 0;
  const ordered = [...groups.values()].sort((a, b) => a.mtime - b.mtime);
  for (const group of ordered) {
    if (total <= maxBytes && now - group.mtime <= maxAgeMs) continue;
    for (const path of group.paths) {
      const bytes = statSize(path);
      try {
        rmSync(path, { force: true });
        removed += bytes;
      } catch {
        // The file may have been removed between listing and pruning.
      }
    }
    total -= group.size;
  }
  return removed;
}

function statSize(path: string): number {
  try {
    return statSync(path).size;
  } catch {
    return 0;
  }
}

export class CaptureStore {
  readonly #dir: string;
  readonly #edge: CaptureEdge;
  readonly #terminalForCompanion: (companionId: string) => string | undefined;
  readonly #companionForTerminal: (terminalId: string) => string | undefined;
  readonly #now: () => number;
  readonly #log: (line: string) => void;
  readonly #retentionMs: number;
  readonly #maxBytes: number;
  readonly #active = new Map<string, CaptureJob>(); // companion -> job
  readonly #done = new Map<string, CaptureDone>();
  readonly #transcribing = new Set<string>();
  readonly #subscribers = new Set<() => void>();

  constructor(options: CaptureStoreOptions) {
    this.#dir = join(options.dir);
    this.#edge = options.edge;
    this.#terminalForCompanion = options.terminalForCompanion;
    this.#companionForTerminal = options.companionForTerminal;
    this.#now = options.now ?? (() => Date.now());
    this.#log = options.log ?? (() => undefined);
    this.#retentionMs = options.retentionMs ?? CAPTURE_RETENTION_MS;
    this.#maxBytes = options.maxBytes ?? CAPTURE_MAX_BYTES;
    mkdirSync(this.#dir, { recursive: true, mode: 0o700 });
    this.#edge.on("capture.progress", (event) => this.#progress(event));
    this.#edge.on("capture.done", (event) => this.#finish(event));
    this.prune();
  }

  get dir(): string {
    return this.#dir;
  }

  /** Capture progress/done changes for push-driven remote SSE. */
  subscribe(listener: () => void): () => void {
    this.#subscribers.add(listener);
    return () => {
      this.#subscribers.delete(listener);
    };
  }

  #notify(): void {
    for (const listener of this.#subscribers) {
      try {
        listener();
      } catch {
        // A broken SSE client must not break the capture job.
      }
    }
  }

  prune(): number {
    try {
      return pruneCaptures(this.#dir, this.#retentionMs, this.#maxBytes);
    } catch (error) {
      this.#log(`capture prune: ${errMessage(error)}`);
      return 0;
    }
  }

  #progress(event: EdgeCaptureProgress): void {
    const companionId = this.#companionForTerminal(event.terminal_id);
    if (companionId === undefined) return;
    const job = this.#active.get(companionId);
    if (job === undefined) return;
    const remaining = Math.max(0, Math.min(job.seconds, Math.floor(event.remaining_s)));
    if (remaining !== job.remaining) {
      job.remaining = remaining;
      this.#notify();
    }
  }

  #finish(event: EdgeCaptureDone): void {
    const companionId = this.#companionForTerminal(event.terminal_id);
    if (companionId === undefined) return;
    const job = this.#active.get(companionId);
    if (job === undefined) return;
    this.#active.delete(companionId);
    const name = basename(event.path).replace(/\.wav$/, "");
    this.#done.set(companionId, {
      name,
      ok: event.error === undefined,
      at: this.#now(),
      ...(event.error === undefined ? {} : { error: event.error }),
    });
    this.prune();
    job.resolve({
      name,
      seconds: event.seconds,
      path: event.path,
      ...(event.error === undefined ? {} : { error: event.error }),
    });
    this.#notify();
  }

  status(companionId: string): CaptureStatus {
    const job = this.#active.get(companionId);
    if (job !== undefined) {
      const elapsed = (this.#now() - job.startedAt) / 1000;
      const remaining = job.remaining > 0 ? job.remaining : Math.max(0, Math.min(job.seconds, Math.ceil(job.seconds - elapsed)));
      return {
        state: "recording",
        id: job.name,
        name: job.name,
        seconds: job.seconds,
        remaining,
        started_at: new Date(job.startedAt).toISOString(),
      };
    }
    const done = this.#done.get(companionId);
    if (done !== undefined && this.#now() - done.at < CAPTURE_DONE_WINDOW_MS) {
      return {
        state: "done",
        id: done.name,
        name: done.name,
        ok: done.ok,
        ...(done.error === undefined ? {} : { error: done.error }),
      };
    }
    return { state: "idle" };
  }

  captureForCompanion(companionId: string): boolean {
    return this.#active.has(companionId);
  }

  async start(companionId: string, seconds: number): Promise<{ id: string; name: string; seconds: number; started_at: string }> {
    if (!Number.isInteger(seconds) || seconds < 1 || seconds > 60) {
      throw new RemoteError(400, "Expected seconds from 1 to 60");
    }
    if (this.#active.has(companionId)) throw new RemoteError(409, "A capture is already running for this companion");
    const terminalId = this.#terminalForCompanion(companionId);
    if (terminalId === undefined) throw new RemoteError(409, "No listening device available for this companion");
    mkdirSync(this.#dir, { recursive: true, mode: 0o700 });
    const startedAt = this.#now();
    const name = uniqueCaptureName(companionId, new Date(startedAt), this.#dir);
    const outPath = join(this.#dir, `${name}.wav`);
    const resolve = Promise.withResolvers<{ name: string; seconds: number; path: string; error?: string }>();
    const job: CaptureJob = { name, companionId, seconds, remaining: seconds, startedAt, terminalId, outPath, resolve: resolve.resolve };
    this.#active.set(companionId, job);
    this.#done.delete(companionId);
    try {
      await this.#edge.captureStart(terminalId, outPath, seconds);
    } catch (error) {
      this.#active.delete(companionId);
      throw error;
    }
    void resolve.promise.then((result) => {
      if (result.error !== undefined) this.#log(`capture ${result.name}: ${result.error}`);
    });
    this.#notify();
    return { id: name, name, seconds, started_at: new Date(startedAt).toISOString() };
  }

  async wait(companionId: string, timeoutMs: number): Promise<{ name: string; seconds: number; path: string; error?: string }> {
    const job = this.#active.get(companionId);
    if (job === undefined) throw new RemoteError(409, "No capture is running");
    return await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new RemoteError(504, "Capture timed out")), timeoutMs);
      const original = job.resolve;
      job.resolve = (result) => {
        clearTimeout(timer);
        original(result);
        resolve(result);
      };
    });
  }

  list(companionId: string): CaptureItem[] {
    if (!existsSync(this.#dir)) return [];
    const out: CaptureItem[] = [];
    for (const entry of readdirSync(this.#dir, { withFileTypes: true })) {
      if (entry.isDirectory() || !entry.name.endsWith(".wav")) continue;
      if (captureOwner(entry.name) !== companionId) continue;
      const path = join(this.#dir, entry.name);
      let info;
      try {
        info = statSync(path);
      } catch {
        continue;
      }
      const name = captureId(entry.name);
      const transcriptPath = join(this.#dir, `${name}.txt`);
      const transcript = existsSync(transcriptPath) ? readFileSync(transcriptPath, "utf8").trim() : "";
      out.push({
        name,
        seconds: Math.max(0, Math.round((info.size - 44) / 32000)),
        size: info.size,
        created: new Date(info.mtimeMs).toISOString(),
        ...(transcript === "" ? {} : { transcript }),
      });
    }
    out.sort((a, b) => b.created.localeCompare(a.created));
    return out.slice(0, 200);
  }

  resolvePath(companionId: string, name: string): string {
    const safe = basename(name);
    if (safe !== name || captureOwner(`${safe}.wav`) !== companionId) {
      throw new RemoteError(404, "Capture not found");
    }
    const path = join(this.#dir, `${safe}.wav`);
    if (!existsSync(path)) throw new RemoteError(404, "Capture not found");
    return path;
  }

  async transcribe(companionId: string, name: string): Promise<{ name: string; transcript: string }> {
    const path = this.resolvePath(companionId, name);
    if (this.#transcribing.has(name)) throw new RemoteError(409, "This capture is already being transcribed");
    if (this.#edge.transcribe === undefined) throw new RemoteError(503, "Transcription unavailable");
    this.#transcribing.add(name);
    try {
      const transcript = (await this.#edge.transcribe(path)).trim();
      writeFileSync(join(this.#dir, `${name}.txt`), `${transcript}\n`, { mode: 0o600 });
      return { name, transcript };
    } finally {
      this.#transcribing.delete(name);
    }
  }

  remove(companionId: string, name: string): void {
    const path = this.resolvePath(companionId, name);
    rmSync(path, { force: true });
    rmSync(join(this.#dir, `${name}.txt`), { force: true });
    this.#notify();
  }
}

function uniqueCaptureName(companionId: string, at: Date, dir: string): string {
  const pad = (value: number): string => String(value).padStart(2, "0");
  const stamp = `${at.getUTCFullYear()}${pad(at.getUTCMonth() + 1)}${pad(at.getUTCDate())}-${pad(at.getUTCHours())}${pad(at.getUTCMinutes())}${pad(at.getUTCSeconds())}`;
  const base = `capture-${companionId}-${stamp}`;
  for (let i = 1; ; i++) {
    const name = i === 1 ? base : `${base}-${i}`;
    if (!existsSync(join(dir, `${name}.wav`))) return name;
  }
}

