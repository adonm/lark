import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { FakeEdge, settle } from "./testfakes.ts";
import { parseTwee } from "./stories.ts";
import type { UiContext } from "./router_ui.ts";
import { handlePointer, openStory, spokenStoryChoice } from "./router_ui.ts";
import type { TerminalState } from "./router_internal.ts";

const TWEE =
  ":: StoryTitle\nLantern\n\n:: Start\nA small lantern waits.\n[[Pick it up->Inside]]\n\n:: Inside {\"end\": true}\nThe lantern glows.\n";

function harness(): { ctx: UiContext; state: TerminalState; edge: FakeEdge; db: LarkDb } {
  const dir = mkdtempSync(join(tmpdir(), "lark-ui-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", form: "a chameleon", configured: true });
  db.saveStory({ id: "lantern", title: "Lantern", twee: TWEE, author: "", updatedAt: 1, passages: 2 });
  const edge = new FakeEdge();
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
  };
  const ctx: UiContext = {
    db,
    edge,
    dc: undefined,
    portraits: undefined,
    terminals: () => [state],
    now: () => 1_760_000_000_000,
    log: () => undefined,
    notify: () => undefined,
    clearPortraitCache: () => undefined,
    storySummaries: () =>
      db.listStories().map((row) => ({ id: row.id, title: row.title, passages: row.passages, progress: 0 })),
    parseStory: (row) => parseTwee(row.twee),
    armListening: async (target) => {
      target.listening = true;
    },
  };
  return { ctx, state, edge, db };
}

test("menu pointer navigation clamps pages and unwinds one drawer at a time", async () => {
  const { ctx, state, edge } = harness();
  await handlePointer(ctx, state, "menu", "");
  assert.equal(state.drawer, "menu");
  assert.equal(state.page, 0);

  await handlePointer(ctx, state, "menu-prev", "2");
  assert.equal(state.page, 2);
  // An empty value falls back to prev/next semantics.
  await handlePointer(ctx, state, "menu-next", "");
  assert.equal(state.page, 1);
  await handlePointer(ctx, state, "menu-page", "-5");
  assert.equal(state.page, 0);

  state.drawer = "volume";
  await handlePointer(ctx, state, "menu-back", "");
  assert.equal(state.drawer, "more", "volume backs out to More");
  await handlePointer(ctx, state, "menu-back", "");
  assert.equal(state.drawer, "menu");
  await handlePointer(ctx, state, "menu-back", "");
  assert.equal(state.drawer, "");
  assert.equal(edge.lastScreen()?.kind, "home");
});

test("volume buttons clamp at both ends and mirror the edge", async () => {
  const { ctx, state, edge, db } = harness();
  db.setTerminalVolume(state.id, 95);
  await handlePointer(ctx, state, "vol-up", "");
  await handlePointer(ctx, state, "vol-up", "");
  assert.equal(db.getTerminalVolume(state.id), 100);
  assert.equal(edge.volumes.at(-1)?.level, 100);

  db.setTerminalVolume(state.id, 5);
  await handlePointer(ctx, state, "vol-down", "");
  await handlePointer(ctx, state, "vol-down", "");
  assert.equal(db.getTerminalVolume(state.id), 0);
  assert.equal(edge.lastScreen()?.kind, "volume");
});

test("taps advance stories silently and ignore unknown choices", async () => {
  const { ctx, state, edge, db } = harness();
  await openStory(ctx, "greg", "lantern", false);
  const before = edge.spoken.length;

  await handlePointer(ctx, state, "choice-99", "");
  assert.equal(state.story?.body, "A small lantern waits.");

  await handlePointer(ctx, state, "choice-0", "1");
  assert.equal(edge.lastScreen()?.body, "The lantern glows.");
  assert.equal(db.getStoryProgress("greg", "lantern")?.passage, "Inside");
  assert.equal(edge.spoken.length, before, "taps must never synthesize the passage");

  // A choice with no open story is a no-op rather than an error.
  state.story = undefined;
  await handlePointer(ctx, state, "choice-0", "1");
});

test("spoken choices match labels exactly enough and leave other speech alone", async () => {
  const { ctx, state, edge } = harness();
  const before = edge.spoken.length;
  await openStory(ctx, "greg", "lantern", false);
  assert.equal(await spokenStoryChoice(ctx, state, "pick it up"), true);
  assert.equal(state.story?.body, "The lantern glows.");
  assert.equal(await spokenStoryChoice(ctx, state, "make me a sandwich"), false);
  assert.equal(edge.spoken.length, before);
});

test("stories resume saved progress and restart resets it", async () => {
  const { ctx, state, edge, db } = harness();
  await openStory(ctx, "greg", "lantern", false);
  await handlePointer(ctx, state, "choice-0", "1");
  assert.equal(db.getStoryProgress("greg", "lantern")?.passage, "Inside");

  await openStory(ctx, "greg", "lantern", false);
  assert.equal(edge.lastScreen()?.body, "The lantern glows.", "resume at the saved passage");

  await openStory(ctx, "greg", "lantern", true);
  assert.equal(edge.lastScreen()?.body, "A small lantern waits.");
  assert.equal(db.getStoryProgress("greg", "lantern")?.passage, "Start");
});

test("the reader's back button returns to the list and clears the run", async () => {
  const { ctx, state, edge } = harness();
  state.drawer = "pair";
  state.previousBrightness = 40;
  await handlePointer(ctx, state, "pair-close", "");
  assert.equal(edge.powers.at(-1)?.brightness, 40, "leaving pairing restores brightness");

  await openStory(ctx, "greg", "lantern", false);
  assert.equal(state.drawer, "story-reader");
  await handlePointer(ctx, state, "menu-back", "");
  assert.equal(state.story, undefined);
  assert.equal(state.drawer, "stories");
  assert.equal(edge.lastScreen()?.title, "Stories");
  await settle();
});
