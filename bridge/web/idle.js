export const idleTiming = {hiddenMs:45_000, inactiveMs:5 * 60_000};
const busyModes = new Set(["call", "calling", "listening", "speaking", "thinking"]);
export const busyMode = mode => busyModes.has(mode);

export function idleWatch({isBusy, onIdle, timing = idleTiming, now = Date.now,
  schedule = setTimeout, cancel = clearTimeout}) {
  let running = false, busy = false, hiddenAt = null, lastActivity, deadline, timer;

  function stop() {
    if (deadline !== undefined) cancel(timer);
    deadline = undefined;
    running = false;
  }

  function refresh() {
    if (!running) return;
    const time = now(), active = isBusy();
    if (busy && !active) {
      // Start a fresh grace period after the companion finishes a turn or call.
      if (hiddenAt !== null) hiddenAt = time;
      lastActivity = time;
    }
    busy = active;
    const due = active ? undefined : Math.min(lastActivity + timing.inactiveMs,
      hiddenAt === null ? Infinity : hiddenAt + timing.hiddenMs);
    if (due !== undefined && due <= time) {
      stop();
      onIdle();
      return;
    }
    if (due === deadline) return;
    if (deadline !== undefined) cancel(timer);
    deadline = due;
    if (due !== undefined) timer = schedule(() => {
      deadline = undefined;
      refresh();
    }, due - time);
  }

  return {
    start(hidden) {
      stop();
      running = true;
      busy = false;
      lastActivity = now();
      hiddenAt = hidden ? lastActivity : null;
      refresh();
    },
    stop, refresh,
    activity() {
      lastActivity = now();
      refresh();
    },
    visibility(hidden) {
      if (!running || hidden === (hiddenAt !== null)) return;
      // Mobile browsers can suspend timers while the page is hidden.
      refresh();
      if (!running) return;
      hiddenAt = hidden ? now() : null;
      if (!hidden) lastActivity = now();
      refresh();
    },
  };
}
