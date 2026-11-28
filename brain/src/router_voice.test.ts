import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { FakeBrain, FakeEdge, settle } from "./testfakes.ts";
import type { VoiceContext } from "./router_voice.ts";
import {
  FOLLOW_UP_GRACE_MS,
  LISTEN_WINDOW_MS,
  armListening,
  cancel,
  commit,
  formatTurnPrompt,
  playbackDone,
  speculate,
  wakeTimer,
} from "./router_voice.ts";
import type { Timer } from "./timers.ts";
import { NOT_CAUGHT_LINE, type TerminalState } from "./router_internal.ts";
import type { StoryRun } from "./router_internal.ts";

function harness(options: { listening?: boolean } = {}): {
  ctx: VoiceContext;
  state: TerminalState;
  edge: FakeEdge;
  brain: FakeBrain;
  db: LarkDb;
  now: { value: number };
} {
  const dir = mkdtempSync(join(tmpdir(), "lark-voice-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({
    id: "greg",
    name: "Greg",
    form: "a chameleon",
    configured: true,
    listening: options.listening ?? false,
  });
  const edge = new FakeEdge();
  const brain = new FakeBrain();
  const now = { value: 1_760_000_000_000 };
  const state: TerminalState = {
    id: "vocat-1",
    companionId: "greg",
    info: { terminal_id: "vocat-1", board: "esp-vocat", firmware: "0.24.0", width: 360, height: 360, shape: "round", ip: "" },
    listening: false,
    listeningSince: 0,
    page: 0,
    drawer: "",
    busy: false,
    speaking: false,
    caption: "",
    previousBrightness: 100,
    specSeq: 0,
  };
  let stream = 0;
  const ctx: VoiceContext = {
    db,
    edge,
    brain,
    dc: undefined,
    identifySong: undefined,
    terminals: () => [state],
    terminal: (id) => (id === state.id ? state : undefined),
    onlineTerminals: () => [state],
    now: () => now.value,
    log: () => undefined,
    notify: () => undefined,
    closed: () => false,
    renderHome: (target, status, caption) => edge.render(target.id, { kind: "home", status, caption }),
    spokenStoryChoice: async () => false,
    nextStream: () => ++stream,
  };
  return { ctx, state, edge, brain, db, now };
}

test("arming the listening window sends its bounds and expires to idle", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = harness();
  await armListening(h.ctx, h.state);
  assert.equal(h.state.listening, true);
  assert.equal(h.edge.lastScreen()?.status, "listening");
  assert.deepEqual(h.edge.recStarts[0]?.options, {
    listenWindow: true,
    windowS: Math.round(LISTEN_WINDOW_MS / 1000),
    followUpGraceMs: FOLLOW_UP_GRACE_MS,
    companionName: "Greg",
  });

  t.mock.timers.tick(LISTEN_WINDOW_MS + 2000);
  await settle();
  assert.equal(h.state.listening, false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("the edge-owned tap-to-talk arm skips rec_start", async () => {
  const h = harness();
  await armListening(h.ctx, h.state, false);
  assert.equal(h.state.listening, true);
  assert.equal(h.edge.recStarts.length, 0);
  if (h.state.listenTimer !== undefined) clearTimeout(h.state.listenTimer);
});

test("after the follow-up grace only speech naming the companion is a turn", async () => {
  const h = harness({ listening: true });
  await armListening(h.ctx, h.state);
  const arms = h.edge.recStarts.length;

  h.now.value += FOLLOW_UP_GRACE_MS + 1000;
  await commit(h.ctx, { terminal_id: "vocat-1", seq: 1, transcript: "what time is it", speech_at_ms: 0, listen_window: true, audio_ms: 700 });
  assert.equal(h.brain.inputs.length, 0, "nameless background chatter must not reach the model");
  assert.equal(h.edge.recStarts.length, arms + 1, "background chatter re-arms instead");

  h.now.value += FOLLOW_UP_GRACE_MS + 1001;
  await commit(h.ctx, { terminal_id: "vocat-1", seq: 2, transcript: "Greg, what time is it", speech_at_ms: 0, listen_window: true, audio_ms: 700 });
  assert.match(
    h.brain.inputs.at(-1)?.text ?? "",
    /^Speaker: in person \(name unknown\)\nGreg, what time is it\n\[state: /,
  );
  if (h.state.listenTimer !== undefined) clearTimeout(h.state.listenTimer);
});

test("speech that began inside the grace still commits after a slow turn", async () => {
  const h = harness({ listening: true });
  await armListening(h.ctx, h.state);
  const since = h.state.listeningSince;

  // Speech starts 8 s in but the capture only commits at 12 s: the grace is
  // measured from onset, so this is a follow-up, not background chatter.
  h.now.value = since + 12_000;
  await commit(h.ctx, {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "what time is it",
    speech_at_ms: since + 8000,
    listen_window: true,
    audio_ms: 4000,
  });
  assert.equal(h.brain.inputs.length, 1, "speech that began inside the grace must reach the model");
  assert.ok(
    h.edge.screens.some((entry) => entry.screen.status === "thinking"),
    "the turn must render thinking",
  );
  if (h.state.listenTimer !== undefined) clearTimeout(h.state.listenTimer);
});

test("unaddressed speculation after the grace does not start a turn", async () => {
  const h = harness({ listening: true });
  await armListening(h.ctx, h.state);
  const since = h.state.listeningSince;

  h.now.value = since + 15_000;
  await speculate(h.ctx, {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "background chatter",
    speech_at_ms: since + 14_000,
  });
  assert.equal(h.brain.speculated.length, 0, "late unaddressed chatter must not burn a speculative turn");

  h.now.value = since + 16_000;
  await speculate(h.ctx, {
    terminal_id: "vocat-1",
    seq: 2,
    transcript: "Greg, are you there",
    speech_at_ms: since + 15_000,
  });
  assert.equal(h.brain.speculated.length, 1, "speech naming the companion still speculates");
  if (h.state.listenTimer !== undefined) clearTimeout(h.state.listenTimer);
});

test("commit clears the window timer so a slow turn keeps thinking", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const h = harness({ listening: true });
  await armListening(h.ctx, h.state);
  assert.notEqual(h.state.listenTimer, undefined);

  const gate = Promise.withResolvers<void>();
  const base = h.brain.runTurn.bind(h.brain);
  h.brain.runTurn = async function* (input: { sessionID: string; text: string }) {
    await gate.promise;
    return yield* base(input);
  };

  const since = h.state.listeningSince;
  h.now.value = since + 12_000;
  const turn = commit(h.ctx, {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "what time is it",
    speech_at_ms: since + 8000,
    listen_window: true,
    audio_ms: 4000,
  });
  await settle();
  assert.equal(h.state.listenTimer, undefined, "commit must clear the window timer");
  assert.equal(h.edge.lastScreen()?.status, "thinking");

  t.mock.timers.tick(LISTEN_WINDOW_MS + 5000);
  await settle();
  assert.equal(h.edge.lastScreen()?.status, "thinking", "the stale window timer must not render idle");

  gate.resolve();
  await turn;
});

test("the silent story reader matches choices and never synthesizes passages", async () => {
  const h = harness();
  h.state.drawer = "story-reader";
  h.state.story = { id: "lantern" } as unknown as StoryRun;
  h.ctx.spokenStoryChoice = async (_state, transcript) => transcript === "pick it up";
  const before = h.edge.spoken.length;

  await commit(h.ctx, { terminal_id: "vocat-1", seq: 1, transcript: "pick it up", speech_at_ms: 0, listen_window: false, audio_ms: 400 });
  assert.equal(h.brain.inputs.length, 0);
  assert.equal(h.edge.spoken.length, before);

  await commit(h.ctx, { terminal_id: "vocat-1", seq: 2, transcript: "stop", speech_at_ms: 0, listen_window: false, audio_ms: 400 });
  assert.equal(h.state.story, undefined);
  assert.equal(h.state.drawer, "");
  assert.equal(h.edge.lastScreen()?.status, "idle");
  assert.equal(h.edge.spoken.length, before);
});

test("speculation ignores blank, busy and reader turns and replaces the previous guess", async () => {
  const h = harness();
  const base = { terminal_id: "vocat-1", seq: 1, speech_at_ms: 0 };
  await speculate(h.ctx, { ...base, transcript: "   " });
  assert.equal(h.brain.speculated.length, 0);

  h.state.busy = true;
  await speculate(h.ctx, { ...base, transcript: "hello" });
  assert.equal(h.brain.speculated.length, 0);

  h.state.busy = false;
  h.state.drawer = "story-reader";
  await speculate(h.ctx, { ...base, transcript: "hello" });
  assert.equal(h.brain.speculated.length, 0);

  h.state.drawer = "";
  await speculate(h.ctx, { ...base, transcript: " first guess " });
  assert.equal(h.brain.speculated.length, 1);
  assert.equal(h.state.speculation?.transcript, "first guess");
  assert.ok(h.brain.speculated[0]?.transcript.includes("first guess"));

  await speculate(h.ctx, { ...base, seq: 2, transcript: "second guess" });
  assert.equal(h.brain.speculated.length, 2);
  assert.equal(h.brain.aborted.length, 1);
  assert.ok(h.brain.aborted[0]?.includes("first guess"));
});

test("playback.done ignores stale and identify streams and clears on the live one", async () => {
  const h = harness();
  h.state.audio = "identify";
  h.state.speaking = true;
  await playbackDone(h.ctx, { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  assert.equal(h.state.audio, "identify", "identify has no playback stream");

  h.state.audio = "tune";
  h.state.audioStreamId = 7;
  await playbackDone(h.ctx, { terminal_id: "vocat-1", stream_id: 3, aborted: false, underruns_delta: 0 });
  assert.equal(h.state.audio, "tune", "a replaced stream must not end the tune");

  await playbackDone(h.ctx, { terminal_id: "vocat-1", stream_id: 7, aborted: false, underruns_delta: 0 });
  assert.equal(h.state.audio, undefined);
  assert.equal(h.state.speaking, false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("cancel aborts in-flight speculation and turn work", async () => {
  const h = harness();
  await speculate(h.ctx, { terminal_id: "vocat-1", seq: 1, transcript: "guess", speech_at_ms: 0 });
  const controller = new AbortController();
  h.state.turn = controller;
  h.state.busy = true;
  await cancel(h.ctx, "vocat-1", "user stopped");
  assert.equal(h.brain.aborted.length, 1);
  assert.ok(h.brain.aborted[0]?.includes("guess"));
  assert.equal(controller.signal.aborted, true);
  assert.equal(h.state.busy, false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("overlapping speculation starts only the newest and never after commit", async () => {
  const h = harness();
  const base = { terminal_id: "vocat-1", speech_at_ms: 0 };
  await Promise.all([
    speculate(h.ctx, { ...base, seq: 1, transcript: "first guess" }),
    speculate(h.ctx, { ...base, seq: 2, transcript: "second guess" }),
  ]);
  assert.equal(h.brain.speculated.length, 1);
  assert.ok(h.brain.speculated[0]?.transcript.includes("second guess"));

  const pending = speculate(h.ctx, { ...base, seq: 3, transcript: "late guess" });
  const committed = commit(h.ctx, {
    ...base,
    seq: 4,
    transcript: "hello there",
    listen_window: false,
    audio_ms: 600,
  });
  await Promise.all([pending, committed]);
  assert.equal(h.brain.speculated.length, 1, "a stale speculation started after commit");
  assert.ok(h.brain.inputs.at(-1)?.text.includes("hello there"));
});

test("an empty matched speculation falls back to a fresh turn", async () => {
  const h = harness();
  const event = { terminal_id: "vocat-1", seq: 1, transcript: "what is it", speech_at_ms: 0, listen_window: false, audio_ms: 600 };
  h.brain.nextTurn("", []);
  await speculate(h.ctx, event);
  h.brain.nextTurn("Recovered.");
  await commit(h.ctx, event);
  assert.equal(h.brain.inputs.length, 2, "the turn was dropped instead of retried");
  assert.ok(h.brain.inputs.at(-1)?.text.includes("what is it"));
  assert.equal(h.state.caption, "Recovered.");
  assert.ok(h.edge.spoken.some((line) => line.text.includes("Recovered")));
});

test("a failed matched speculation falls back to a fresh turn", async () => {
  const h = harness();
  const event = { terminal_id: "vocat-1", seq: 1, transcript: "what is it", speech_at_ms: 0, listen_window: false, audio_ms: 600 };
  h.brain.speculationErrors.push(new Error("provider exploded"));
  await speculate(h.ctx, event);
  h.brain.nextTurn("Recovered after failure.");
  await commit(h.ctx, event);
  assert.equal(h.brain.inputs.length, 2);
  assert.equal(h.state.caption, "Recovered after failure.");
});

test("an empty reply says it was not caught instead of staying silent", async () => {
  const h = harness();
  h.brain.nextTurn("", []);
  await commit(h.ctx, {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello?",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  assert.equal(h.state.caption, NOT_CAUGHT_LINE);
  assert.ok(h.edge.spoken.some((line) => line.text.includes(NOT_CAUGHT_LINE)));
});

test("turn prompts carry the live clock, form, volume, battery and timers", () => {
  const h = harness();
  h.db.patchCompanion("greg", { voice: "bm_fable", listening: true });
  h.db.setTerminalVolume("vocat-1", 60);
  h.state.telemetry = {
    terminal_id: "vocat-1",
    wifi_rssi: -40,
    battery_pct: 82,
    charging: true,
    cpu_pct: 5,
    free_heap: 1,
    uptime_s: 1,
  };
  h.db.insertTimer({
    id: "t1",
    companionId: "greg",
    label: "Pasta",
    say: "",
    chime: "tap",
    wakeAgent: true,
    createdAt: new Date(h.now.value).toISOString(),
    fireAt: new Date(h.now.value + 4 * 60_000).toISOString(),
    notes: [],
  });
  const companion = h.db.getCompanion("greg");
  assert.ok(companion !== undefined);
  const lines = formatTurnPrompt(h.ctx, { text: "what now", companion, state: h.state }).split("\n");
  assert.equal(lines[0], "Speaker: in person (name unknown)");
  assert.equal(lines[1], "what now");
  assert.match(
    lines[2] ?? "",
    /^\[state: now: \w{3} \d{4}-\d{2}-\d{2} \d{1,2}:\d{2} (AM|PM) \(.+\) · form: a chameleon · voice: bm_fable · listening: on · volume: 60% · battery: 82% charging · timers: 1 active: "Pasta" 4m left\]$/,
  );
});

test("a BOX-3 on USB reports USB instead of a negative battery reading", () => {
  const h = harness();
  h.state.info = { ...h.state.info, board: "esp-box-3" };
  h.state.telemetry = {
    terminal_id: "vocat-1",
    wifi_rssi: -40,
    battery_pct: -1,
    charging: false,
    cpu_pct: 5,
    free_heap: 1,
    uptime_s: 1,
  };
  const companion = h.db.getCompanion("greg");
  assert.ok(companion !== undefined);
  const prompt = formatTurnPrompt(h.ctx, { text: "hi", companion, state: h.state });
  assert.match(prompt, /· battery: USB ·/);
  assert.equal(prompt.includes("battery: -1%"), false);
});

test("state values cannot terminate the footer and an offline terminal is explicit", () => {
  const h = harness();
  h.db.patchCompanion("greg", { form: "a fox ] in a box" });
  const companion = h.db.getCompanion("greg");
  assert.ok(companion !== undefined);
  const prompt = formatTurnPrompt(h.ctx, { text: "hi", companion, replyTo: "Sam" });
  assert.ok(prompt.startsWith("Speaker: in person (name unknown), replying to Sam (Delta Chat)\nhi\n[state: "));
  assert.match(prompt, /· form: a fox \) in a box ·/);
  assert.equal(prompt.match(/\[state: /g)?.length, 1);
  assert.ok(prompt.endsWith("]"));
  assert.match(prompt, /· terminal: offline · timers: none\]$/);
});

test("an armed Delta Chat reply names the contact on the next voice turn", async () => {
  const h = harness();
  h.state.reply = {
    accountId: 1,
    chatId: 77,
    contact: "Sam",
    peer: "sam@example.org",
    armedUntil: h.now.value + 60_000,
  };
  await commit(h.ctx, {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "tell Sam hello",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  assert.match(
    h.brain.inputs.at(-1)?.text ?? "",
    /^Speaker: in person \(name unknown\), replying to Sam \(Delta Chat\)\ntell Sam hello\n\[state: /,
  );
  const stored = h.db
    .listMessages({ companionId: "greg", limit: 5 })
    .messages.find((message) => message.role === "user");
  assert.equal(stored?.text, "tell Sam hello", "history keeps only the clean transcript");
});

test("a wake-agent timer runs an event turn and falls back to its line", async () => {
  const h = harness();
  const timer: Timer = {
    id: "t1",
    companionId: "greg",
    label: "Pasta",
    say: "Tea is ready.",
    chime: "tap",
    wakeAgent: true,
    notes: [],
    createdAt: new Date(h.now.value),
    fireAt: new Date(h.now.value),
  };
  h.brain.nextTurn("Your pasta is done.", ["Your pasta is done."]);
  await wakeTimer(h.ctx, timer);
  assert.match(
    h.brain.inputs.at(-1)?.text ?? "",
    /^Event: timer "Pasta" fired: Tea is ready\.\n\n\[state: /,
  );
  assert.ok(h.edge.spoken.some((line) => line.text.includes("Your pasta is done")));

  h.brain.nextTurn("", []);
  await wakeTimer(h.ctx, timer);
  assert.ok(h.edge.spoken.at(-1)?.text.includes("Tea is ready."));
});

test("cancel with reason=speech aborts speculation without clearing listening", async () => {
  const h = harness();
  await armListening(h.ctx, h.state, false);
  h.brain.nextTurn("Half guess.", ["Half guess."]);
  await speculate(h.ctx, { terminal_id: "vocat-1", seq: 4, transcript: "half", speech_at_ms: 0 });
  await cancel(h.ctx, "vocat-1", "speech");
  assert.equal(h.brain.aborted.length, 1);
  assert.ok(h.brain.aborted[0]?.includes("half"));
  assert.equal(h.state.listening, true);
  assert.equal(h.edge.lastScreen()?.status, "listening");

  await cancel(h.ctx, "vocat-1", "noise");
  assert.equal(h.state.listening, false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
});
