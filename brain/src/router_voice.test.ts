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
  playbackDone,
  speculate,
} from "./router_voice.ts";
import { NOT_CAUGHT_LINE, type TerminalState } from "./router_internal.ts";
import type { StoryRun } from "./router_internal.ts";

function harness(options: { listening?: boolean } = {}): {
  ctx: VoiceContext;
  state: TerminalState;
  edge: FakeEdge;
  brain: FakeBrain;
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
  return { ctx, state, edge, brain, now };
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
  assert.equal(h.brain.inputs.at(-1)?.text, "Greg, what time is it");
  if (h.state.listenTimer !== undefined) clearTimeout(h.state.listenTimer);
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
  assert.equal(h.brain.speculated[0]?.transcript, "first guess");

  await speculate(h.ctx, { ...base, seq: 2, transcript: "second guess" });
  assert.equal(h.brain.speculated.length, 2);
  assert.deepEqual(h.brain.aborted, ["first guess"]);
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
  assert.deepEqual(h.brain.aborted, ["guess"]);
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
  assert.equal(h.brain.speculated[0]?.transcript, "second guess");

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
  assert.equal(h.brain.inputs.at(-1)?.text, "hello there");
});

test("an empty matched speculation falls back to a fresh turn", async () => {
  const h = harness();
  const event = { terminal_id: "vocat-1", seq: 1, transcript: "what is it", speech_at_ms: 0, listen_window: false, audio_ms: 600 };
  h.brain.nextTurn("", []);
  await speculate(h.ctx, event);
  h.brain.nextTurn("Recovered.");
  await commit(h.ctx, event);
  assert.equal(h.brain.inputs.length, 2, "the turn was dropped instead of retried");
  assert.equal(h.brain.inputs.at(-1)?.text, "what is it");
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
