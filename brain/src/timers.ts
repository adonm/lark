// Timer storage, two-minute duplicate collapse, local-time deadlines, and the
// active scheduler. The row shape matches the Go server's `timers` table.
import { randomBytes } from "node:crypto";
import type { LarkDb, TimerRow } from "./db.ts";

export const TIMER_DEDUP_WINDOW_MS = 2 * 60 * 1000;
export const TIMER_EXPIRY_MS = 2 * 60 * 1000;
export const TIMER_GRACE_MS = 2000;
export const TIMER_MAX_SECONDS = 86400;
export const TIMER_WAKE_MIN_SECONDS = 30;
export const TIMER_LIMIT_PER_DEVICE = 8;
export const TIMER_LIMIT_TOTAL = 32;
export const TIMER_RECENT_MS = 10 * 60 * 1000;
const TIMER_RECENT_MAX = 64;

export const CHIMES = ["tap", "think", "intercom", "celebrate"] as const;
export type Chime = (typeof CHIMES)[number];

export class TimerError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "TimerError";
    this.status = status;
  }
}

export interface Timer {
  id: string;
  companionId: string;
  label: string;
  say: string;
  chime: string;
  wakeAgent: boolean;
  notes: string[];
  createdAt: Date;
  fireAt: Date;
}

export interface TimerState {
  id: string;
  companion_id: string;
  label: string;
  say: string;
  notes: string[];
  wake_agent: boolean;
  created_at: Date;
  chime: string;
  fire_at: Date;
  fire_at_local: string;
  remaining_s: number;
}

export interface TimerEdit {
  id?: string;
  inSeconds?: number;
  at?: string;
  label?: string;
  say?: string;
  chime?: string;
  wakeAgent?: boolean;
  notes?: string | readonly string[];
}

export interface SetTimerInput extends TimerEdit {
  companionId: string;
}

export interface SchedulerOptions {
  onFire: (timer: Timer) => void;
  now?: () => Date;
  /** Shortest accepted in_seconds; tests lower it, production keeps 1. */
  minSeconds?: number;
  dedupWindowMs?: number;
  maxPerDevice?: number;
  maxTotal?: number;
}

interface Handle {
  timer: ReturnType<typeof setTimeout>;
  fireAt: number;
}

function randomID(): string {
  return randomBytes(12).toString("hex");
}

function normalizeLabel(label: string): string {
  return label.trim().toLowerCase();
}

function toTimer(row: TimerRow): Timer {
  return {
    id: row.id,
    companionId: row.companionId,
    label: row.label,
    say: row.say,
    chime: row.chime,
    wakeAgent: row.wakeAgent,
    notes: row.notes,
    createdAt: new Date(row.createdAt),
    fireAt: new Date(row.fireAt),
  };
}

function toRow(timer: Timer): TimerRow {
  return {
    id: timer.id,
    companionId: timer.companionId,
    label: timer.label,
    say: timer.say,
    chime: timer.chime,
    wakeAgent: timer.wakeAgent,
    notes: timer.notes,
    createdAt: timer.createdAt.toISOString(),
    fireAt: timer.fireAt.toISOString(),
  };
}

function pad(value: number): string {
  return String(value).padStart(2, "0");
}

export function formatLocalTime(date: Date): string {
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

function localParts(raw: string): number[] | undefined {
  const match = /^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2}))?$/.exec(raw);
  if (match === null) return undefined;
  return [
    Number(match[1]),
    Number(match[2]),
    Number(match[3]),
    Number(match[4]),
    Number(match[5]),
    Number(match[6] ?? 0),
  ];
}

/** RFC3339 or a local wall-clock time (`2026-11-26T07:30`, space allowed). */
export function parseTimerAt(raw: string): Date {
  const value = raw.trim();
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) {
    const parsed = new Date(value);
    if (!Number.isNaN(parsed.getTime())) return parsed;
  }
  const parts = localParts(value);
  if (parts !== undefined) {
    const [year, month, day, hour, minute, second] = parts as [number, number, number, number, number, number];
    const date = new Date(year, month - 1, day, hour, minute, second, 0);
    if (
      date.getFullYear() !== year ||
      date.getMonth() !== month - 1 ||
      date.getDate() !== day ||
      date.getHours() !== hour ||
      date.getMinutes() !== minute ||
      date.getSeconds() !== second
    ) {
      throw new TimerError(400, "at must be a valid local time");
    }
    return date;
  }
  throw new TimerError(400, "invalid timer timestamp");
}

function parseNotes(value: string | readonly string[] | undefined): string[] {
  if (value === undefined) return [];
  if (typeof value === "string") {
    const parts = value.split(/\s+/).filter(Boolean);
    for (const part of parts) {
      if (!/^[A-Ga-g][#b]?\d{1,2}:\d{1,5}$/.test(part)) {
        throw new TimerError(400, "timer notes must look like C5:150 E5:150");
      }
    }
    return parts;
  }
  return value.map((part) => {
    if (!/^[A-Ga-g][#b]?\d{1,2}:\d{1,5}$/.test(part)) {
      throw new TimerError(400, "timer notes must look like C5:150 E5:150");
    }
    return part;
  });
}

function shortText(value: string, max: number): string {
  return value.trim().replace(/\s+/g, " ").slice(0, max);
}

export class TimerScheduler {
  readonly #db: LarkDb;
  readonly #onFire: (timer: Timer) => void;
  readonly #now: () => Date;
  readonly #minSeconds: number;
  readonly #dedupWindowMs: number;
  readonly #maxPerDevice: number;
  readonly #maxTotal: number;
  readonly #handles = new Map<string, Handle>();
  // Timers the scheduler has just fired, so a late update/cancel can explain
  // that the deadline already passed instead of "not found".
  readonly #recentlyFired = new Map<string, { companionId: string; label: string; firedAtMs: number }>();
  #started = false;
  #closed = false;

  constructor(db: LarkDb, options: SchedulerOptions) {
    this.#db = db;
    this.#onFire = options.onFire;
    this.#now = options.now ?? (() => new Date());
    this.#minSeconds = Math.max(0.001, options.minSeconds ?? 1);
    this.#dedupWindowMs = options.dedupWindowMs ?? TIMER_DEDUP_WINDOW_MS;
    this.#maxPerDevice = options.maxPerDevice ?? TIMER_LIMIT_PER_DEVICE;
    this.#maxTotal = options.maxTotal ?? TIMER_LIMIT_TOTAL;
  }

  /** Restore persisted timers and arm every deadline. */
  start(): void {
    if (this.#started) return;
    this.#started = true;
    const now = this.#now();
    for (const row of this.#db.listTimers()) {
      const timer = toTimer(row);
      const delay = timer.fireAt.getTime() - now.getTime();
      if (delay < -TIMER_EXPIRY_MS) {
        this.#db.deleteTimer(timer.id);
      } else if (delay <= 0) {
        this.#arm(timer.id, TIMER_GRACE_MS);
      } else {
        this.#arm(timer.id, delay);
      }
    }
  }

  stop(): void {
    this.#closed = true;
    for (const handle of this.#handles.values()) clearTimeout(handle.timer);
    this.#handles.clear();
  }

  list(companionId?: string): TimerState[] {
    return this.#db
      .listTimers(companionId)
      .map(toTimer)
      .sort((a, b) => a.fireAt.getTime() - b.fireAt.getTime())
      .map((timer) => this.state(timer));
  }

  get(id: string): Timer | undefined {
    const row = this.#db.getTimer(id);
    return row === undefined ? undefined : toTimer(row);
  }

  state(timer: Timer, now = this.#now()): TimerState {
    const remaining = Math.max(0, timer.fireAt.getTime() - now.getTime());
    return {
      id: timer.id,
      companion_id: timer.companionId,
      label: timer.label,
      say: timer.say,
      notes: timer.notes,
      wake_agent: timer.wakeAgent,
      created_at: timer.createdAt,
      chime: timer.chime,
      fire_at: timer.fireAt,
      fire_at_local: formatLocalTime(timer.fireAt),
      remaining_s: Math.ceil(remaining / 1000),
    };
  }

  /** Set a new timer, collapsing a same-label deadline within two minutes. */
  set(input: SetTimerInput): TimerState {
    if (this.#closed) throw new TimerError(503, "Timers unavailable");
    const now = this.#now();
    const companionId = input.companionId.toLowerCase();
    if (input.inSeconds !== undefined && input.at !== undefined) {
      throw new TimerError(400, "Provide exactly one of in_seconds or at");
    }
    if (input.inSeconds === undefined && input.at === undefined) {
      throw new TimerError(400, "Provide exactly one of in_seconds or at");
    }
    const fireAt = this.#deadline(input, now);
    const label = shortText(input.label ?? "Timer", 60) || "Timer";
    const chime = input.chime ?? "celebrate";
    if (!CHIMES.includes(chime as Chime)) throw new TimerError(400, "Unknown chime kind");
    const say = shortText(input.say ?? "", 600);
    const wakeAgent = input.wakeAgent ?? false;
    this.#validateWake(wakeAgent, fireAt, now);
    const notes = parseNotes(input.notes);

    const normalized = normalizeLabel(label);
    if (normalized !== "") {
      for (const row of this.#db.listTimers(companionId)) {
        const other = toTimer(row);
        if (normalizeLabel(other.label) !== normalized) continue;
        if (Math.abs(other.fireAt.getTime() - fireAt.getTime()) > this.#dedupWindowMs) continue;
        // Two near-identical spoken requests are one timer; the later deadline wins.
        const updated: Timer = { ...other, label, say, chime, wakeAgent, fireAt, notes };
        this.#db.updateTimer(toRow(updated));
        this.#arm(updated.id, Math.max(0, fireAt.getTime() - now.getTime()));
        return this.state(updated, now);
      }
    }

    if (this.#db.countTimers(companionId) >= this.#maxPerDevice) {
      throw new TimerError(
        409,
        `Timer limit reached (${this.#maxPerDevice} per device, ${this.#maxTotal} total)`,
      );
    }
    if (this.#db.countTimers() >= this.#maxTotal) {
      throw new TimerError(
        409,
        `Timer limit reached (${this.#maxPerDevice} per device, ${this.#maxTotal} total)`,
      );
    }

    const timer: Timer = {
      id: randomID(),
      companionId,
      label,
      say,
      chime,
      wakeAgent,
      notes,
      createdAt: now,
      fireAt,
    };
    this.#db.insertTimer(toRow(timer));
    this.#arm(timer.id, Math.max(0, fireAt.getTime() - now.getTime()));
    return this.state(timer, now);
  }

  update(id: string, companionId: string, edit: TimerEdit): TimerState {
    if (this.#closed) throw new TimerError(503, "Timers unavailable");
    const existing = this.get(id);
    if (existing === undefined) throw this.#missing(id, companionId);
    if (existing.companionId !== companionId.toLowerCase()) {
      throw new TimerError(403, "Companion out of scope");
    }
    const now = this.#now();
    let timer = { ...existing };
    if (edit.inSeconds !== undefined && edit.at !== undefined) {
      throw new TimerError(400, "Provide exactly one of in_seconds or at");
    }
    if (edit.inSeconds !== undefined || edit.at !== undefined) {
      timer.fireAt = this.#deadline(edit, now);
    }
    if (edit.wakeAgent !== undefined) timer.wakeAgent = edit.wakeAgent;
    this.#validateWake(timer.wakeAgent, timer.fireAt, now);
    if (edit.label !== undefined) timer.label = shortText(edit.label, 60) || "Timer";
    if (edit.say !== undefined) timer.say = shortText(edit.say, 600);
    if (edit.chime !== undefined) {
      if (!CHIMES.includes(edit.chime as Chime)) throw new TimerError(400, "Unknown chime kind");
      timer.chime = edit.chime;
    }
    if (edit.notes !== undefined) timer.notes = parseNotes(edit.notes);
    this.#db.updateTimer(toRow(timer));
    this.#arm(timer.id, Math.max(0, timer.fireAt.getTime() - now.getTime()));
    return this.state(timer, now);
  }

  cancel(companionId: string, options: { id?: string; all?: boolean } = {}): string[] {
    if (this.#closed) throw new TimerError(503, "Timers unavailable");
    const id = options.id;
    const all = options.all === true;
    if (all === Boolean(id)) throw new TimerError(400, "Provide exactly one of id or all=true");
    if (!all && id !== undefined) {
      const timer = this.get(id);
      if (timer === undefined) throw this.#missing(id, companionId);
      if (timer.companionId !== companionId.toLowerCase()) {
        throw new TimerError(403, "Companion out of scope");
      }
      this.#db.deleteTimer(id);
      this.#disarm(id);
      return [timer.companionId];
    }
    const ids = this.#db
      .listTimers(companionId)
      .map((row) => row.id);
    for (const timerId of ids) {
      this.#db.deleteTimer(timerId);
      this.#disarm(timerId);
    }
    return ids;
  }

  #rememberFired(timer: Timer): void {
    const firedAtMs = this.#now().getTime();
    this.#recentlyFired.delete(timer.id);
    this.#recentlyFired.set(timer.id, { companionId: timer.companionId, label: timer.label, firedAtMs });
    while (this.#recentlyFired.size > TIMER_RECENT_MAX) {
      const oldest = this.#recentlyFired.keys().next().value;
      if (oldest === undefined) break;
      this.#recentlyFired.delete(oldest);
    }
  }

  #missing(id: string, companionId: string): TimerError {
    const recent = this.#recentlyFired.get(id);
    if (recent === undefined) return new TimerError(404, "Timer not found");
    const ageMs = this.#now().getTime() - recent.firedAtMs;
    if (ageMs > TIMER_RECENT_MS || recent.companionId !== companionId.toLowerCase()) {
      this.#recentlyFired.delete(id);
      return new TimerError(404, "Timer not found");
    }
    const ago = Math.max(0, Math.round(ageMs / 1000));
    return new TimerError(404, `Timer "${recent.label}" already finished ${ago}s ago; set a new timer instead`);
  }

  #deadline(edit: TimerEdit, now: Date): Date {
    if (edit.inSeconds !== undefined) {
      const seconds = edit.inSeconds;
      if (!Number.isFinite(seconds) || seconds < this.#minSeconds || seconds > TIMER_MAX_SECONDS) {
        throw new TimerError(400, `in_seconds must be ${this.#minSeconds}–${TIMER_MAX_SECONDS}`);
      }
      return new Date(now.getTime() + seconds * 1000);
    }
    if (edit.at === undefined) throw new TimerError(400, "Provide exactly one of in_seconds or at");
    const fireAt = parseTimerAt(edit.at);
    const delay = fireAt.getTime() - now.getTime();
    if (delay <= 0 || delay > 24 * 60 * 60 * 1000) {
      throw new TimerError(400, "at must be RFC3339 or local time within the next 24 hours");
    }
    return fireAt;
  }

  #validateWake(wakeAgent: boolean, fireAt: Date, now: Date): void {
    if (!wakeAgent) return;
    if (fireAt.getTime() - now.getTime() < TIMER_WAKE_MIN_SECONDS * 1000) {
      throw new TimerError(400, "wake_agent requires in_seconds of at least 30");
    }
  }

  #arm(id: string, delay: number): void {
    this.#disarm(id);
    const safeDelay = Math.max(0, Math.min(2 ** 31 - 1, Math.round(delay)));
    const timer = setTimeout(() => {
      this.#handles.delete(id);
      const claimed = this.#db.claimTimer(id);
      if (claimed === undefined) return;
      this.#rememberFired(toTimer(claimed));
      this.#onFire(toTimer(claimed));
    }, safeDelay);
    this.#handles.set(id, { timer, fireAt: delay });
  }

  #disarm(id: string): void {
    const handle = this.#handles.get(id);
    if (handle !== undefined) {
      clearTimeout(handle.timer);
      this.#handles.delete(id);
    }
  }
}
