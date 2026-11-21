import assert from "node:assert/strict";
import {test} from "node:test";
import {busyMode, idleTiming, idleWatch} from "./idle.js";

function fixture() {
  let time = 0, due, task, pauses = 0, mode = "idle", recording = false;
  const watch = idleWatch({
    isBusy: () => recording || busyMode(mode),
    onIdle: () => pauses++,
    now: () => time,
    schedule(cb, ms) { due = time + ms; task = cb; return 1; },
    cancel() { task = null; },
  });
  watch.start(false);
  return {
    watch,
    get pauses() { return pauses; },
    mode(value) { mode = value; watch.refresh(); },
    recording(value) { recording = value; watch.refresh(); },
    advance(ms, suspended = false) {
      time += ms;
      if (task && time >= due && !suspended) {
        const run = task;
        task = null;
        run();
      }
    },
  };
}

test("interaction renews the visible deadline, pause does not auto-resume", () => {
  const f = fixture(), delay = idleTiming.inactiveMs;
  f.advance(delay - 1);
  f.watch.activity();
  f.advance(delay - 1);
  assert.equal(f.pauses, 0);
  f.advance(1);
  f.watch.activity();
  f.watch.visibility(false);
  f.advance(delay);
  assert.equal(f.pauses, 1);
  f.watch.start(false);
  f.advance(delay);
  assert.equal(f.pauses, 2);
});

test("short visibility change is safe; suspended hidden timer is checked on return", () => {
  const f = fixture(), delay = idleTiming.hiddenMs;
  f.watch.visibility(true);
  f.advance(delay - 1);
  f.watch.visibility(false);
  f.advance(delay);
  assert.equal(f.pauses, 0);
  f.watch.visibility(true);
  f.advance(delay, true);
  f.watch.visibility(false);
  assert.equal(f.pauses, 1);
});

for (const mode of ["call", "calling", "listening", "thinking", "speaking", "local recording"]) {
  test(`${mode} prevents background and inactivity disconnects and gets a fresh grace period`, () => {
    const f = fixture(), delay = idleTiming.hiddenMs;
    if (mode === "local recording") f.recording(true);
    else f.mode(mode);
    f.advance(idleTiming.inactiveMs);
    f.watch.visibility(true);
    f.advance(delay * 2);
    assert.equal(f.pauses, 0);
    if (mode === "local recording") f.recording(false);
    else f.mode("idle");
    f.advance(delay - 1);
    assert.equal(f.pauses, 0);
    f.advance(1);
    assert.equal(f.pauses, 1);
  });
}
