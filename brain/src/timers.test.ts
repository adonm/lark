import assert from "node:assert/strict";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import {
  TIMER_DEDUP_WINDOW_MS,
  TimerError,
  TimerScheduler,
  formatLocalTime,
  parseTimerAt,
} from "./timers.ts";
import type { Timer } from "./timers.ts";

function freshDb(): LarkDb {
  return new LarkDb(":memory:");
}

test("two-minute same-label requests collapse onto one row", () => {
  const db = freshDb();
  let now = new Date("2026-11-26T07:00:00");
  const fired: Timer[] = [];
  const scheduler = new TimerScheduler(db, { onFire: (timer) => fired.push(timer), now: () => now });
  const first = scheduler.set({ companionId: "Cake", label: "tea", inSeconds: 300 });
  now = new Date(now.getTime() + 30_000);
  const second = scheduler.set({ companionId: "cake", label: "Tea", inSeconds: 360 });
  assert.equal(second.id, first.id);
  assert.equal(second.fire_at.getTime(), now.getTime() + 360_000);
  assert.equal(scheduler.list("cake").length, 1);

  now = new Date(now.getTime() + TIMER_DEDUP_WINDOW_MS + 120_000);
  const third = scheduler.set({ companionId: "cake", label: "tea", inSeconds: 300 });
  assert.notEqual(third.id, first.id);
  assert.equal(scheduler.list("cake").length, 2);
  assert.equal(db.countTimers(), 2);
  scheduler.stop();
  assert.equal(fired.length, 0);
  db.close();
});

test("local wall-clock and RFC3339 deadlines parse", () => {
  const local = parseTimerAt("2026-11-26T07:30");
  assert.equal(local.getFullYear(), 2026);
  assert.equal(local.getMonth(), 10);
  assert.equal(local.getDate(), 26);
  assert.equal(local.getHours(), 7);
  assert.equal(local.getMinutes(), 30);
  assert.equal(formatLocalTime(local), "2026-11-26T07:30:00");

  const spaced = parseTimerAt("2026-11-26 07:30:15");
  assert.equal(spaced.getSeconds(), 15);
  const zoned = parseTimerAt("2026-11-26T07:30:00Z");
  assert.equal(zoned.toISOString(), "2026-11-26T07:30:00.000Z");
  assert.throws(() => parseTimerAt("tomorrow"), TimerError);
  assert.throws(() => parseTimerAt("2026-02-30T07:30"), TimerError);
});

test("set, update, list, and cancel keep the companion scope", () => {
  const db = freshDb();
  const now = new Date("2026-11-26T07:00:00");
  const scheduler = new TimerScheduler(db, { onFire: () => undefined, now: () => now });
  const set = scheduler.set({
    companionId: "cake",
    inSeconds: 600,
    label: "oven",
    say: "The oven is ready",
    notes: "C5:150 E5:150",
  });
  assert.equal(set.label, "oven");
  assert.equal(set.remaining_s, 600);
  assert.deepEqual(set.notes, ["C5:150", "E5:150"]);

  const updated = scheduler.update(set.id, "cake", { inSeconds: 700, label: "Oven ready" });
  assert.equal(updated.id, set.id);
  assert.equal(updated.remaining_s, 700);
  assert.equal(updated.label, "Oven ready");

  assert.throws(() => scheduler.update(set.id, "biscuit", { label: "no" }), /out of scope/);
  assert.throws(() => scheduler.update("missing", "cake", { label: "no" }), /not found/);
  assert.throws(() => scheduler.cancel("cake", {}), /exactly one/);
  assert.throws(() => scheduler.set({ companionId: "cake", inSeconds: 10, chime: "gong" }), /chime/);
  assert.throws(() => scheduler.set({ companionId: "cake", inSeconds: 10, wakeAgent: true }), /at least 30/);

  scheduler.cancel("cake", { id: set.id });
  assert.equal(scheduler.list("cake").length, 0);

  scheduler.set({ companionId: "cake", inSeconds: 60, label: "a" });
  scheduler.set({ companionId: "cake", inSeconds: 120, label: "b" });
  const cancelled = scheduler.cancel("cake", { all: true });
  assert.equal(cancelled.length, 2);
  assert.equal(scheduler.list("cake").length, 0);
  scheduler.stop();
  db.close();
});

test("per-device limits reject a ninth timer", () => {
  const db = freshDb();
  const scheduler = new TimerScheduler(db, {
    onFire: () => undefined,
    now: () => new Date(),
    maxPerDevice: 2,
    maxTotal: 4,
  });
  scheduler.set({ companionId: "cake", inSeconds: 60, label: "one" });
  scheduler.set({ companionId: "cake", inSeconds: 61, label: "two" });
  assert.throws(
    () => scheduler.set({ companionId: "cake", inSeconds: 62, label: "three" }),
    /Timer limit reached/,
  );
  scheduler.stop();
  db.close();
});

test("the scheduler emits a fire callback and removes the row", async () => {
  const db = freshDb();
  const fired: Timer[] = [];
  const scheduler = new TimerScheduler(db, {
    minSeconds: 0.01,
    onFire: (timer) => fired.push(timer),
  });
  scheduler.start();
  const timer = scheduler.set({ companionId: "cake", inSeconds: 0.03, label: "quick", say: "ding" });
  await new Promise((resolve) => setTimeout(resolve, 120));
  assert.equal(fired.length, 1);
  assert.equal(fired[0]?.id, timer.id);
  assert.equal(fired[0]?.say, "ding");
  assert.equal(db.countTimers(), 0);
  assert.equal(scheduler.list("cake").length, 0);
  scheduler.stop();
  db.close();
});

test("start restores a persisted timer and fires it", async () => {
  const db = freshDb();
  const now = new Date();
  const first = new TimerScheduler(db, { onFire: () => undefined, now: () => now, minSeconds: 0.01 });
  const timer = first.set({ companionId: "cake", inSeconds: 0.05, label: "restore" });
  first.stop();

  const fired: Timer[] = [];
  const restored = new TimerScheduler(db, {
    onFire: (value) => fired.push(value),
    now: () => now,
    minSeconds: 0.01,
  });
  restored.start();
  await new Promise((resolve) => setTimeout(resolve, 140));
  assert.equal(fired.length, 1);
  assert.equal(fired[0]?.id, timer.id);
  restored.stop();
  db.close();
});

