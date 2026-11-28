import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { Router, nameMatches, sanitizeSpeech, type RouterDc } from "./router.ts";
import { FakeBrain, FakeEdge, settle } from "./testfakes.ts";

const STORY =
  ":: StoryTitle\nLantern\n\n:: Start\nA small lantern waits.\n[[Pick it up->Inside]]\n\n:: Inside {\"end\": true}\nThe lantern glows.\n";

const TERMINAL = {
  terminal_id: "vocat-1",
  board: "esp-vocat",
  firmware: "0.21.0",
  width: 360,
  height: 360,
  shape: "round",
  ip: "127.0.0.1",
};

class FakeDc implements RouterDc {
  readonly sent: Array<{ accountId: number; chatId: number; text: string }> = [];
  readonly unpaired: number[] = [];
  paired = [{ id: 10, name: "Sam", address: "sam@example.org" }];
  invite = "https://i.delta.chat/#abc&a=greg";

  async sendText(accountId: number, chatId: number, text: string): Promise<void> {
    this.sent.push({ accountId, chatId, text });
  }

  async listPairedContacts(): Promise<Array<{ id: number; name: string; address: string }>> {
    return this.paired;
  }

  async unpairContact(_companionId: string, contactId: number): Promise<void> {
    this.unpaired.push(contactId);
    this.paired = this.paired.filter((contact) => contact.id !== contactId);
  }

  async pairInvite(): Promise<string> {
    return this.invite;
  }

  accountForCompanion(): number {
    return 1;
  }
}

interface Harness {
  dir: string;
  db: LarkDb;
  edge: FakeEdge;
  brain: FakeBrain;
  dc: FakeDc;
  router: Router;
  now: { value: number };
}

function harness(options: {
  listening?: boolean;
  portrait?: string;
  identifySong?: (wavPath: string) => Promise<{ matched: boolean; title?: string; artist?: string; album?: string }>;
} = {}): Harness {
  const dir = mkdtempSync(join(tmpdir(), "lark-router-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({
    id: "greg",
    name: "Greg",
    form: "an old chameleon",
    personality: "dry",
    voice: "af_heart",
    configured: true,
    listening: options.listening ?? false,
    portraitId: options.portrait ?? "",
  });
  db.saveStory({ id: "lantern", title: "Lantern", twee: STORY, author: "", updatedAt: 1, passages: 2 });
  const edge = new FakeEdge();
  const brain = new FakeBrain();
  const dc = new FakeDc();
  const now = { value: 1_760_000_000_000 };
  const router = new Router({
    db,
    edge,
    brain,
    dataDir: dir,
    dc,
    ...(options.identifySong === undefined ? {} : { identifySong: options.identifySong }),
    ...(options.portrait === undefined ? {} : { portraits: { dirFor: (portraitId: string) => join(dir, "portraits", portraitId), request: async () => options.portrait ?? "", available: true } }),
    now: () => now.value,
    log: () => undefined,
  });
  return { dir, db, edge, brain, dc, router, now };
}

async function connect(h: Harness): Promise<void> {
  h.edge.emit("ready", { models: { parakeet: { state: "ready" }, kokoro: { state: "ready" } } });
  h.edge.emit("terminal.connected", TERMINAL);
  await settle();
}

test("connected terminal renders the pixel home screen", async () => {
  const h = harness();
  await connect(h);
  const screen = h.edge.lastScreen();
  assert.equal(screen?.kind, "home");
  assert.equal(screen?.companion_name, "Greg");
  assert.equal(screen?.companion_form, "an old chameleon");
  assert.equal(screen?.status, "idle");
  assert.equal(h.db.getTerminal("vocat-1")?.companionId, "greg");
});

test("menu opens stories and the story reader stays silent", async () => {
  const h = harness();
  await connect(h);
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu", value: "" });
  await settle();
  assert.equal(h.edge.lastScreen()?.kind, "menu");

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-stories", value: "stories" });
  await settle();
  const list = h.edge.lastScreen();
  assert.equal(list?.kind, "list");
  assert.equal(list?.title, "Stories");
  assert.equal(list?.items?.[0]?.id, "lantern");

  const before = h.edge.spoken.length;
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "lantern", value: "lantern" });
  await settle();
  const reader = h.edge.lastScreen();
  assert.equal(reader?.kind, "reader");
  assert.equal(reader?.title, "Lantern");
  assert.equal(reader?.body, "A small lantern waits.");
  assert.equal(h.edge.spoken.length, before, "opening a story must not synthesize speech");
  assert.equal(h.db.getStoryProgress("greg", "lantern")?.passage, "Start");
});

test("story choices advance silently, by tap and by voice", async () => {
  const h = harness();
  await connect(h);
  await h.router.openStory("greg", "lantern", false);
  const before = h.edge.spoken.length;

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "choice-0", value: "1" });
  await settle();
  assert.equal(h.edge.lastScreen()?.body, "The lantern glows.");
  assert.equal(h.db.getStoryProgress("greg", "lantern")?.passage, "Inside");

  await h.router.restartStory("greg", "lantern");
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "pick it up",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle();
  assert.equal(h.edge.lastScreen()?.body, "The lantern glows.");
  assert.equal(h.edge.spoken.length, before, "voice choice matching must remain silent");
});

test("a committed turn streams speech and speculation is promoted", async () => {
  const h = harness();
  await connect(h);
  h.brain.nextTurn("Hello there. How are you?", ["Hello ", "there. ", "How are you?"]);

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "listen", value: "" });
  await settle();
  // The edge opens the tap-to-talk capture itself; the brain only mirrors the
  // listening state and re-arms with rec_start after a reply.
  assert.equal(h.edge.recStarts.length, 0);
  assert.equal(h.edge.lastScreen()?.status, "listening");

  h.edge.emit("turn.speculate", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello greg",
    speech_at_ms: 100,
  });
  await settle();
  assert.equal(h.brain.speculated.length, 1);

  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello greg",
    speech_at_ms: 100,
    listen_window: false,
    audio_ms: 900,
  });
  await settle(20);

  assert.equal(h.brain.inputs.at(-1)?.text, "hello greg");
  assert.deepEqual(h.brain.promoted, ["hello greg"]);
  assert.ok(h.edge.starts.length >= 1);
  const text = h.edge.spoken.map((chunk) => chunk.text).join(" ");
  assert.match(text, /Hello/);
  assert.match(text, /How are you/);
  assert.ok(h.edge.ends.length >= 1);
  assert.equal(h.edge.lastScreen()?.status, "speaking");

  h.edge.emit("playback.done", { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  await settle();
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("leaving speaking clears the stale caption and restores the menu", async () => {
  const h = harness();
  await connect(h);
  h.brain.nextTurn("Hello there.", ["Hello there."]);

  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello greg",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 700,
  });
  await settle(20);
  assert.equal(h.edge.lastScreen()?.status, "speaking");
  assert.equal(h.edge.lastScreen()?.caption, "Hello there.");

  // A state refresh while speaking keeps the spoken caption.
  await h.router.refreshCompanion("greg");
  assert.equal(h.edge.lastScreen()?.caption, "Hello there.");

  // Playback completion must clear it, or the idle menu stays covered.
  h.edge.emit("playback.done", { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  await settle();
  assert.equal(h.edge.lastScreen()?.status, "idle");
  assert.equal(h.edge.lastScreen()?.caption, "");

  // Thinking and listening never inherit the previous reply either.
  await h.router.setListening("greg", true);
  assert.equal(h.edge.lastScreen()?.status, "listening");
  assert.equal(h.edge.lastScreen()?.caption, "");
  await h.router.setListening("greg", false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
  assert.equal(h.edge.lastScreen()?.caption, "");
});

test("a stale stored session binding is replaced", async () => {
  const h = harness();
  h.db.putSession({ companionId: "greg", sessionId: "ses_stale", updatedAt: 1, contractHash: "", contextAfterMs: 0 });
  h.brain.missingSessions.add("ses_stale");
  h.brain.nextTurn("Fresh session reply.", ["Fresh session reply."]);
  await connect(h);
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello greg",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle(20);
  assert.deepEqual(h.brain.sessions, [{ id: "greg", name: "Greg", personality: "dry" }]);
  assert.equal(h.db.getSession("greg"), undefined, "the stale row is dropped");
  assert.equal(h.brain.inputs.at(-1)?.sessionID, "sess-greg");
  assert.equal(h.brain.sessionExistsCalls, 0, "turns must not pre-probe the session RPC");
  assert.match(h.edge.spoken.map((chunk) => chunk.text).join(" "), /Fresh session/);
});

test("a healthy stored session is trusted without a per-turn probe", async () => {
  const h = harness();
  h.db.putSession({ companionId: "greg", sessionId: "ses_live", updatedAt: 1, contractHash: "", contextAfterMs: 0 });
  h.brain.nextTurn("Reused session reply.", ["Reused session reply."]);
  await connect(h);
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello again",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle(20);
  assert.equal(h.brain.sessionExistsCalls, 0);
  assert.deepEqual(h.brain.sessions, []);
  assert.equal(h.brain.inputs.at(-1)?.sessionID, "ses_live");
  assert.match(h.edge.spoken.map((chunk) => chunk.text).join(" "), /Reused session/);
});

test("a mismatched commit aborts speculation and runs a fresh turn", async () => {
  const h = harness();
  await connect(h);
  h.brain.nextTurn("Wrong guess reply.", ["Wrong guess reply."]);
  h.edge.emit("turn.speculate", { terminal_id: "vocat-1", seq: 4, transcript: "wrong guess", speech_at_ms: 0 });
  await settle();
  h.brain.nextTurn("A fresh reply.", ["A fresh reply."]);
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 4,
    transcript: "right question",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle(20);
  assert.deepEqual(h.brain.aborted, ["wrong guess"]);
  assert.deepEqual(h.brain.promoted, []);
  assert.equal(h.brain.inputs.at(-1)?.text, "right question");
  assert.match(h.edge.spoken.map((chunk) => chunk.text).join(" "), /A fresh reply/);
});

test("not-caught speaks the fixed recovery line", async () => {
  const h = harness();
  await connect(h);
  h.edge.emit("turn.not_caught", { terminal_id: "vocat-1" });
  await settle();
  assert.match(h.edge.spoken.map((chunk) => chunk.text).join(" "), /didn't catch that/);
});

test("listening re-arms and requires the name after the follow-up grace", async () => {
  const h = harness({ listening: true });
  await connect(h);
  assert.equal(h.edge.recStarts.length, 1);
  assert.equal(h.edge.lastScreen()?.status, "listening");

  h.now.value += 11_000;
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 2,
    transcript: "what time is it",
    speech_at_ms: 0,
    listen_window: true,
    audio_ms: 700,
  });
  await settle();
  assert.equal(h.brain.inputs.length, 0, "nameless speech after grace is background chatter");
  assert.equal(h.edge.recStarts.length, 2);

  h.now.value += 1000;
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 3,
    transcript: "Greg, what time is it",
    speech_at_ms: 0,
    listen_window: true,
    audio_ms: 700,
  });
  await settle(20);
  assert.equal(h.brain.inputs.length, 1);
  assert.equal(h.brain.inputs[0]?.text, "Greg, what time is it");
});

test("fuzzy names and spoken sanitising", () => {
  assert.equal(nameMatches("gregg are you there", "Greg"), true);
  assert.equal(nameMatches("hey cake", "Greg"), false);
  assert.equal(nameMatches("hello", "Greg"), false);
  assert.equal(sanitizeSpeech("**Hi** [silent] there"), "Hi there");
});

test("one voice turn writes exactly one user row", async () => {
  const h = harness();
  await connect(h);
  h.brain.nextTurn("A single reply.", ["A single reply."]);
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "hello greg",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle(20);
  const rows = h.db.listMessages({ companionId: "greg", limit: 50 }).messages;
  assert.equal(rows.filter((row) => row.role === "user").length, 1);
  assert.equal(rows.filter((row) => row.role === "assistant").length, 1);
  assert.equal(rows.find((row) => row.role === "user")?.text, "hello greg");
});

test("polling snapshots reuse the cached portrait and parsed story", async () => {
  const portraitId = "a".repeat(32);
  const h = harness({ portrait: portraitId });
  const dir = join(h.dir, "portraits", portraitId);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "sheet.png"), Buffer.from("portrait-bytes"));

  const first = h.router.snapshot("greg");
  assert.equal(first.companion.portraitUrl, `/remote/api/portrait?v=${portraitId}`);
  // Deleting the sheet proves the second snapshot used the cached presence.
  rmSync(join(dir, "sheet.png"));
  assert.equal(h.router.snapshot("greg").companion.portraitUrl, first.companion.portraitUrl);
  assert.equal(h.router.snapshot("greg").companion.portraitUrl?.includes("base64"), false);

  h.db.saveStoryProgress({ companionId: "greg", storyId: "lantern", passage: "Start", page: 0, flags: {}, updatedAt: 1 });
  assert.equal(h.router.storySummaries("greg")[0]?.progress, 50);

  // Rewriting the Twee under the same updated_at must not invalidate the cache.
  h.db.saveStory({
    id: "lantern",
    title: "Lantern",
    twee: ":: StoryTitle\nLantern\n\n:: Start\nOne.\n[[a]]\n[[b]]\n[[c]]\n[[d]]\n[[e]]\n\n:: a\nA\n\n:: b\nB\n\n:: c\nC\n\n:: d\nD\n\n:: e\nE\n",
    author: "",
    updatedAt: 1,
    passages: 6,
  });
  assert.equal(h.router.storySummaries("greg")[0]?.progress, 50);
});

test("a device reply within the 60 s window is sent back to Delta Chat", async () => {
  const h = harness();
  await connect(h);
  await h.router.notifyDcTurn({
    companionId: "greg",
    text: "how is the garden",
    sender: "Sam",
    peer: "sam@example.org",
    private: false,
    accountId: 1,
    chatId: 77,
  });
  await settle();
  h.edge.emit("playback.done", { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  await settle();
  h.brain.nextTurn("The tomatoes are ripe.", ["The tomatoes are ripe."]);
  h.edge.emit("turn.commit", {
    terminal_id: "vocat-1",
    seq: 1,
    transcript: "tell Sam the tomatoes are ripe",
    speech_at_ms: 0,
    listen_window: false,
    audio_ms: 500,
  });
  await settle(20);
  assert.deepEqual(h.dc.sent.at(-1), { accountId: 1, chatId: 77, text: "The tomatoes are ripe." });
  const routed = h.db
    .listMessages({ companionId: "greg", channel: "deltachat", limit: 10 })
    .messages.find((message) => message.text === "The tomatoes are ripe.");
  assert.equal(routed?.peer, "sam@example.org");
  assert.equal(routed?.role, "assistant");
});

test("volume, voice and unpair menus call shared commands", async () => {
  const h = harness();
  await connect(h);
  h.db.setTerminalVolume("vocat-1", 50);

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu", value: "" });
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-more", value: "more" });
  await settle();
  assert.equal(h.edge.lastScreen()?.title, "More");

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "volume", value: "volume" });
  await settle();
  assert.equal(h.edge.lastScreen()?.kind, "volume");

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "vol-up", value: "10" });
  await settle();
  assert.equal(h.db.getTerminalVolume("vocat-1"), 60);
  assert.equal(h.edge.volumes.at(-1)?.level, 60);

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-back", value: "" });
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-voice", value: "voice" });
  await settle();
  assert.equal(h.edge.lastScreen()?.kind, "voice");
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "voice-bm_fable", value: "bm_fable" });
  await settle();
  assert.equal(h.db.getCompanion("greg")?.voice, "bm_fable");
  assert.equal(h.edge.previews.at(-1)?.voice, "bm_fable");

  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-back", value: "" });
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-unpair", value: "unpair" });
  await settle();
  const list = h.edge.lastScreen();
  assert.equal(list?.title, "Unpair");
  assert.equal(list?.items?.[0]?.id, "unpair-10");
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "unpair-10", value: "10" });
  await settle();
  assert.deepEqual(h.dc.unpaired, [10]);
});

test("pair menu raises brightness and shows the invite", async () => {
  const h = harness();
  await connect(h);
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu", value: "" });
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "menu-more", value: "more" });
  h.edge.emit("pointer.action", { terminal_id: "vocat-1", action: "pair", value: "pair" });
  await settle();
  assert.equal(h.edge.lastScreen()?.kind, "pair_qr");
  assert.equal(h.edge.lastScreen()?.invite, "https://i.delta.chat/#abc&a=greg");
  assert.equal(h.edge.powers.at(-1)?.brightness, 100);
});

test("lark_audio metronome and tune captions end in a clean home screen", async () => {
  const h = harness();
  await connect(h);

  const metro = await h.router.audioMetronome("greg", 100, 4, 10);
  assert.equal(metro.caption, "Metronome · 100 BPM (4/4)");
  assert.deepEqual(h.edge.metronomes, [{ terminalId: "vocat-1", bpm: 100, beats: 4, durationS: 10 }]);
  assert.equal(h.edge.lastScreen()?.status, "speaking");
  assert.equal(h.edge.lastScreen()?.caption, "Metronome · 100 BPM (4/4)");

  h.edge.emit("playback.done", { terminal_id: "vocat-1", stream_id: 1, aborted: false, underruns_delta: 0 });
  await settle();
  assert.equal(h.edge.lastScreen()?.status, "idle");

  const tune = await h.router.audioTune("greg", 120, "hum", [{ note: "C4", beats: 1 }], "Twinkle");
  assert.equal(tune.caption, "Tune · Twinkle (hum)");
  assert.equal(h.edge.tunes.length, 1);
  assert.deepEqual(h.edge.tunes[0]?.notes, [{ note: "C4", beats: 1 }]);
  assert.equal(h.edge.lastScreen()?.status, "speaking");

  await h.router.audioStop("greg");
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("lark_audio identify captures, recognizes, deletes, and restores home", async () => {
  let seen = "";
  const h = harness({
    identifySong: async (wavPath) => {
      seen = wavPath;
      return { matched: true, title: "Get Lucky", artist: "Daft Punk", album: "Random Access Memories" };
    },
  });
  await connect(h);

  const pending = h.router.audioIdentify("greg", 7);
  await settle(20);
  const capture = h.edge.captures[0];
  assert.ok(capture !== undefined, "capture did not start");
  assert.equal(capture.seconds, 7);
  assert.equal(h.edge.lastScreen()?.status, "listening");
  assert.equal(h.edge.lastScreen()?.caption, "Listening to music...");

  h.edge.emitCaptureDone({ terminal_id: "vocat-1", capture_id: "c1", path: capture.path, seconds: 7 });
  const result = await pending;
  assert.equal(result.matched, true);
  assert.equal(result.title, "Get Lucky");
  assert.equal(result.artist, "Daft Punk");
  assert.equal(result.album, "Random Access Memories");
  assert.equal(seen, capture.path);
  assert.equal(existsSync(capture.path), false, "capture WAV must be removed");
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("lark_audio stop cancels an active identify capture", async () => {
  const h = harness({ identifySong: async () => ({ matched: false }) });
  await connect(h);
  const pending = h.router.audioIdentify("greg", 8);
  await settle(20);
  assert.ok(h.edge.captures[0] !== undefined);
  await h.router.audioStop("greg");
  assert.equal(h.edge.captureStops, 1);
  const capture = h.edge.captures[0];
  assert.ok(capture !== undefined);
  h.edge.emitCaptureDone({ terminal_id: "vocat-1", capture_id: "c1", path: capture.path, seconds: 1 });
  assert.equal((await pending).matched, false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
});

test("remote controls drive the same router commands", async () => {
  const h = harness();
  await connect(h);
  h.brain.nextTurn("Testing remote.", ["Testing remote."]);
  await h.router.sayNow("greg", "hello from remote");
  assert.match(h.edge.spoken.map((chunk) => chunk.text).join(" "), /hello from remote/);
  await h.router.setListening("greg", true);
  assert.equal(h.edge.lastScreen()?.status, "listening");
  await h.router.setListening("greg", false);
  assert.equal(h.edge.lastScreen()?.status, "idle");
  await h.router.openStory("greg", "lantern", false);
  assert.equal(h.edge.lastScreen()?.kind, "reader");
});

test("connecting restores default panel brightness so offline-dimmed boards wake", async () => {
  const h = harness();
  await connect(h);
  assert.deepEqual(h.edge.powers, [{ terminalId: "vocat-1", brightness: 85 }]);
});
