import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
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

function harness(options: { listening?: boolean; portrait?: string } = {}): Harness {
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

test("a device reply within the 60 s window is sent back to Delta Chat", async () => {
  const h = harness();
  await connect(h);
  await h.router.notifyDcTurn({
    companionId: "greg",
    text: "how is the garden",
    sender: "Sam",
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
