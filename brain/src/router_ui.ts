// Screens, drawers, menus, pair/unpair, stories and volume/voice settings.
// The router facade owns lifecycle and state; these functions only mutate the
// terminal state they are handed and call back into the edge and router.
import type { ScreenItem } from "./edge_client.ts";
import type { Companion, LarkDb, StoryRow } from "./db.ts";
import type { RouterDc, RouterEdge, StorySummary } from "./router.ts";
import type { TerminalState } from "./router_internal.ts";
import { evalPassage, flagsMap, flagsSlice, matchChoice, type Story } from "./stories.ts";
import { errMessage } from "./util.ts";
import { statusOf } from "./router_internal.ts";

export const VOICES = [
  { id: "af_heart", name: "Heart" },
  { id: "bm_fable", name: "Fable" },
  { id: "bf_lily", name: "Lily" },
  { id: "bm_george", name: "George" },
  { id: "af_bella", name: "Bella" },
  { id: "am_puck", name: "Puck" },
  { id: "bf_alice", name: "Alice" },
  { id: "bf_isabella", name: "Isabella" },
] as const;

export interface UiContext {
  db: LarkDb;
  edge: RouterEdge;
  dc: RouterDc | undefined;
  portraits: { dirFor(id: string): string | undefined } | undefined;
  terminals: () => Iterable<TerminalState>;
  now: () => number;
  log: (line: string) => void;
  notify: () => void;
  clearPortraitCache: () => void;
  storySummaries: (companionId: string) => StorySummary[];
  parseStory: (row: StoryRow) => Story;
  armListening: (state: TerminalState, startRec?: boolean) => Promise<void>;
}

export function portraitDir(ctx: UiContext, companion: Companion | undefined): string | undefined {
  if (companion === undefined || companion.portraitId === "") return undefined;
  return ctx.portraits?.dirFor(companion.portraitId);
}

export async function renderHome(
  ctx: UiContext,
  state: TerminalState,
  status: string,
  caption = state.caption,
): Promise<void> {
  const companion = ctx.db.getCompanion(state.companionId);
  if (companion === undefined) return;
  state.caption = caption;
  await ctx.edge.render(state.id, {
    kind: "home",
    portrait_dir: portraitDir(ctx, companion),
    companion_name: companion.name,
    companion_form: companion.form,
    status,
    caption,
    listen_window: state.listening,
  });
}

export async function refreshCompanion(ctx: UiContext, companionId: string): Promise<void> {
  // A completed portrait replaces the sheet behind a new id; drop the cached
  // data URLs so a same-id rewrite is also picked up.
  ctx.clearPortraitCache();
  for (const state of ctx.terminals()) {
    if (state.companionId !== companionId) continue;
    if (state.drawer === "") await renderHome(ctx, state, statusOf(state));
  }
  ctx.notify();
}

export async function handlePointer(
  ctx: UiContext,
  state: TerminalState,
  hit: string,
  value: string,
): Promise<void> {
  const companion = ctx.db.getCompanion(state.companionId);
  if (companion === undefined) return;

  if (hit === "listen") {
    // The edge opens the tap-to-talk capture itself; re-issuing rec_start
    // here would cancel the window while the speaker is already talking.
    await ctx.armListening(state, false);
    ctx.edge.chime(state.id, "tap");
    return;
  }
  if (hit === "listen-stop" || hit === "stop") {
    const identifying = state.audio === "identify";
    state.listening = false;
    state.audio = undefined;
    state.audioStreamId = undefined;
    if (identifying) await ctx.edge.captureStop(state.id).catch(() => undefined);
    await ctx.edge.stopAudio(state.id);
    await renderHome(ctx, state, "idle");
    ctx.notify();
    return;
  }
  if (hit === "menu") {
    state.drawer = "menu";
    state.page = 0;
    await renderDrawer(ctx, state);
    return;
  }
  if (hit === "menu-back" || hit === "pair-close") {
    if (state.drawer === "pair") ctx.edge.power(state.id, state.previousBrightness);
    if (state.drawer === "story-reader") {
      state.story = undefined;
      state.drawer = "stories";
    } else if (state.drawer === "menu") {
      state.drawer = "";
      await renderHome(ctx, state, statusOf(state));
      return;
    } else if (["volume", "pair", "unpair", "stats"].includes(state.drawer)) {
      state.drawer = "more";
      state.page = 0;
    } else {
      state.drawer = "menu";
      state.page = 0;
    }
    await renderDrawer(ctx, state);
    return;
  }
  if (hit === "menu-prev" || hit === "menu-next" || hit === "menu-page") {
    const parsed = Number.parseInt(value === "" ? hit.replace("menu-", "") : value, 10);
    state.page = Number.isFinite(parsed) ? Math.max(0, parsed) : Math.max(0, state.page - 1);
    if (state.story !== undefined && state.drawer === "story-reader") {
      state.story.page = state.page;
      await renderStory(ctx, state);
      await saveStoryProgress(ctx, state);
    } else {
      await renderDrawer(ctx, state);
    }
    return;
  }
  if (hit.startsWith("choice-")) {
    const index = Number.parseInt(hit.slice("choice-".length), 10);
    if (Number.isFinite(index)) await chooseStory(ctx, state, index);
    return;
  }
  if (hit === "vol-up" || hit === "vol-down") {
    const delta = hit === "vol-up" ? 10 : -10;
    const level = Math.max(0, Math.min(100, ctx.db.getTerminalVolume(state.id) + delta));
    ctx.db.setTerminalVolume(state.id, level);
    ctx.edge.volume(state.id, level);
    await renderVolume(ctx, state, level);
    ctx.notify();
    return;
  }
  if (state.drawer === "volume" || hit.startsWith("volume")) {
    await renderVolume(ctx, state, ctx.db.getTerminalVolume(state.id));
    return;
  }
  const actionValue = value !== "" ? value : hit.startsWith("menu-") ? hit.slice("menu-".length) : hit;
  if (
    actionValue === "talk" ||
    actionValue === "stories" ||
    actionValue === "memories" ||
    actionValue === "voice" ||
    actionValue === "more" ||
    actionValue === "volume" ||
    actionValue === "pair" ||
    actionValue === "unpair" ||
    actionValue === "stats"
  ) {
    state.drawer = actionValue;
    state.page = 0;
    await renderDrawer(ctx, state);
    return;
  }
  if (state.drawer === "voice") {
    const voiceId = value !== "" ? value : hit;
    await setVoice(ctx, companion.id, voiceId, true);
    await renderVoice(ctx, state);
    return;
  }
  if (state.drawer === "stories") {
    const story = ctx.db.listStories().find((row) => row.id === hit || row.id === value);
    if (story !== undefined) {
      await openStory(ctx, companion.id, story.id, false);
      return;
    }
  }
  if (state.drawer === "unpair" && (hit.startsWith("unpair-") || value !== "")) {
    const contactId = Number.parseInt(value !== "" ? value : hit.replace("unpair-", ""), 10);
    if (Number.isFinite(contactId)) await unpair(ctx, state, contactId);
    return;
  }
  if (state.drawer === "memories" || state.drawer === "talk") {
    await renderDetail(ctx, state, hit);
    return;
  }
  if (actionValue === "companion" || actionValue === "dismiss" || actionValue === "close-card") {
    state.drawer = "";
    await renderHome(ctx, state, statusOf(state));
  }
}

export async function renderDrawer(ctx: UiContext, state: TerminalState): Promise<void> {
  const companion = ctx.db.getCompanion(state.companionId);
  if (companion === undefined) return;
  switch (state.drawer) {
    case "menu":
      await ctx.edge.render(state.id, { kind: "menu", page: state.page });
      return;
    case "more":
      await ctx.edge.render(state.id, {
        kind: "list",
        title: "More",
        page: state.page,
        items: [
          { id: "volume", label: "Volume", detail: `${ctx.db.getTerminalVolume(state.id)}%`, icon: "volume" },
          { id: "pair", label: "Pair", detail: "Scan my code", icon: "pair" },
          { id: "unpair", label: "Unpair", detail: "Remove a contact", icon: "unpair" },
          { id: "stats", label: "Stats", detail: "Device and brain", icon: "stats" },
        ],
      });
      return;
    case "volume":
      await renderVolume(ctx, state, ctx.db.getTerminalVolume(state.id));
      return;
    case "voice":
      await renderVoice(ctx, state);
      return;
    case "stats":
      await ctx.edge.render(state.id, {
        kind: "stats",
        rows: statsRows(ctx, state),
      });
      return;
    case "pair": {
      if (ctx.dc === undefined) {
        await renderDetail(ctx, state, "pair", "Pairing unavailable", "Delta Chat is not running.");
        return;
      }
      state.previousBrightness = 100;
      ctx.edge.power(state.id, 100);
      try {
        const invite = await ctx.dc.pairInvite(companion.id);
        await ctx.edge.render(state.id, {
          kind: "pair_qr",
          companion_name: companion.name,
          invite,
        });
      } catch (error) {
        await renderDetail(ctx, state, "pair", "Pairing unavailable", errMessage(error));
      }
      return;
    }
    case "unpair": {
      const contacts = ctx.dc === undefined ? [] : await ctx.dc.listPairedContacts(companion.id);
      await ctx.edge.render(state.id, {
        kind: "list",
        title: "Unpair",
        page: state.page,
        items: contacts.map((contact) => ({
          id: `unpair-${contact.id}`,
          label: contact.name,
          detail: contact.address,
          icon: "unpair",
          action: "unpair-contact",
          value: String(contact.id),
        })),
      });
      return;
    }
    case "talk":
      await ctx.edge.render(state.id, {
        kind: "list",
        title: "Talk to…",
        page: state.page,
        items: ctx.db
          .listCompanions()
          .filter((peer) => peer.id !== companion.id)
          .map((peer) => ({
            id: `talk-${peer.id}`,
            label: peer.name,
            detail: peer.form,
            icon: "talk",
            action: "talk-select",
            value: peer.id,
          })),
      });
      return;
    case "memories":
      await ctx.edge.render(state.id, {
        kind: "list",
        title: "Memories",
        page: state.page,
        items: memories(ctx, companion.id),
      });
      return;
    case "stories":
      await ctx.edge.render(state.id, {
        kind: "list",
        title: "Stories",
        page: state.page,
        items: ctx.storySummaries(companion.id).map((story) => ({
          id: story.id,
          label: story.title,
          detail: story.progress >= 100 ? "Finished" : story.progress > 0 ? `${story.progress}%` : `${story.passages} passages`,
          icon: "books",
          action: "story-open",
          value: story.id,
        })),
      });
      return;
    default:
      state.drawer = "menu";
      await ctx.edge.render(state.id, { kind: "menu", page: 0 });
  }
}

export async function renderVolume(ctx: UiContext, state: TerminalState, level: number): Promise<void> {
  ctx.edge.volume(state.id, level);
  await ctx.edge.render(state.id, { kind: "volume", level, page: state.page });
}

export async function renderVoice(ctx: UiContext, state: TerminalState): Promise<void> {
  const companion = ctx.db.getCompanion(state.companionId);
  await ctx.edge.render(state.id, {
    kind: "voice",
    current: companion?.voice ?? "af_heart",
    page: state.page,
    voices: VOICES.map((voice) => ({ id: voice.id, label: voice.name })),
  });
}

function memories(ctx: UiContext, companionId: string): ScreenItem[] {
  const rows = [...ctx.db.listMemory("shared"), ...ctx.db.listMemory(`companion:${companionId}`)];
  return rows.slice(0, 24).map((row) => ({
    id: `memory-${row.scope}-${row.key}`,
    label: row.key,
    detail: typeof row.value === "string" ? row.value.slice(0, 80) : JSON.stringify(row.value).slice(0, 80),
    icon: "memories",
  }));
}

function statsRows(ctx: UiContext, state: TerminalState): Array<{ title: string; text: string }> {
  const companion = ctx.db.getCompanion(state.companionId);
  const rows = [
    { title: "Companion", text: companion?.name ?? "—" },
    { title: "Voice", text: companion?.voice === "" ? "Heart" : companion?.voice ?? "—" },
    { title: "Terminal", text: state.info.board === "" ? state.id : `${state.info.board} · ${state.info.firmware}` },
    { title: "Volume", text: `${ctx.db.getTerminalVolume(state.id)}%` },
    { title: "Timers", text: String(ctx.db.countTimers(state.companionId)) },
    { title: "Stories", text: String(ctx.db.listStories().length) },
  ];
  if (state.telemetry !== undefined) {
    rows.push(
      { title: "Battery", text: `${state.telemetry.battery_pct}%${state.telemetry.charging ? " charging" : ""}` },
      { title: "Wi-Fi", text: `${state.telemetry.wifi_rssi} dBm` },
    );
  }
  return rows;
}

export async function renderDetail(
  ctx: UiContext,
  state: TerminalState,
  id: string,
  heading?: string,
  body?: string,
): Promise<void> {
  let title = heading ?? "Details";
  let text = body ?? "";
  if (id.startsWith("memory-")) {
    const row = [...ctx.db.listMemory("shared"), ...ctx.db.listMemory(`companion:${state.companionId}`)].find(
      (memory) => `memory-${memory.scope}-${memory.key}` === id,
    );
    if (row !== undefined) {
      title = row.key;
      text = typeof row.value === "string" ? row.value : JSON.stringify(row.value, null, 2);
    }
  } else if (id.startsWith("talk-")) {
    const peer = ctx.db.getCompanion(id.slice("talk-".length));
    if (peer !== undefined) {
      title = peer.name;
      text = peer.form === "" ? "Say hello on the device." : peer.form;
    }
  }
  await ctx.edge.render(state.id, { kind: "detail", title, heading: title, body: text });
}

async function unpair(ctx: UiContext, state: TerminalState, contactId: number): Promise<void> {
  if (ctx.dc === undefined) return;
  try {
    await ctx.dc.unpairContact(state.companionId, contactId);
    ctx.edge.chime(state.id, "tap");
  } catch (error) {
    ctx.log(`unpair: ${errMessage(error)}`);
  }
  state.drawer = "unpair";
  await renderDrawer(ctx, state);
}

export async function openStory(
  ctx: UiContext,
  companionId: string,
  storyId: string,
  restart: boolean,
): Promise<void> {
  const row = ctx.db.getStory(storyId);
  if (row === undefined) throw new Error(`story ${storyId} not found`);
  const story = ctx.parseStory(row);
  let passage = story.start;
  let flags = flagsSlice(story);
  let page = 0;
  const progress = restart ? undefined : ctx.db.getStoryProgress(companionId, storyId);
  if (progress !== undefined) {
    const index = story.byName.get(progress.passage);
    if (index !== undefined) {
      passage = index;
      flags = flagsSlice(story, progress.flags);
      page = Math.max(0, progress.page);
    }
  }
  if (restart) ctx.db.deleteStoryProgress(companionId, storyId);
  const evaluated = evalPassage(story, passage, flags);
  const run = {
    id: row.id,
    story,
    passage,
    flags,
    title: story.title,
    body: evaluated.text,
    choices: evaluated.choices,
    page,
  };
  for (const state of ctx.terminals()) {
    if (state.companionId !== companionId) continue;
    state.story = run;
    state.drawer = "story-reader";
    state.page = page;
    await ctx.edge.stopAudio(state.id);
    await renderStory(ctx, state);
    await saveStoryProgress(ctx, state);
  }
  ctx.notify();
}

async function renderStory(ctx: UiContext, state: TerminalState): Promise<void> {
  const run = state.story;
  if (run === undefined) return;
  const companion = ctx.db.getCompanion(state.companionId);
  const choices = run.choices.map((choice, index) => ({
    id: `choice-${index}`,
    label: choice.label,
    value: String(choice.target),
  }));
  await ctx.edge.render(state.id, {
    kind: "reader",
    portrait_dir: portraitDir(ctx, companion),
    title: run.title,
    body: run.body,
    choices,
    page: run.page,
  });
}

export async function chooseStory(ctx: UiContext, state: TerminalState, index: number): Promise<void> {
  const run = state.story;
  if (run === undefined) return;
  const choice = run.choices[index];
  if (choice === undefined) return;
  const evaluated = evalPassage(run.story, choice.target, run.flags);
  run.passage = choice.target;
  run.body = evaluated.text;
  run.choices = evaluated.choices;
  run.page = 0;
  state.page = 0;
  await renderStory(ctx, state);
  await saveStoryProgress(ctx, state);
}

export async function saveStoryProgress(ctx: UiContext, state: TerminalState): Promise<void> {
  const run = state.story;
  if (run === undefined) return;
  const passage = run.story.passages[run.passage];
  ctx.db.saveStoryProgress({
    companionId: state.companionId,
    storyId: run.id,
    passage: passage?.name ?? "",
    page: run.page,
    flags: flagsMap(run.story, run.flags),
    updatedAt: ctx.now(),
  });
  ctx.notify();
}

export async function spokenStoryChoice(
  ctx: UiContext,
  state: TerminalState,
  transcript: string,
): Promise<boolean> {
  const run = state.story;
  if (run === undefined || run.choices.length === 0) return false;
  const result = matchChoice(transcript, run.choices);
  if (!result.matched) return false;
  await chooseStory(ctx, state, result.index);
  return true;
}

export async function setVoice(
  ctx: UiContext,
  companionId: string,
  voiceId: string,
  preview: boolean,
): Promise<void> {
  if (!VOICES.some((voice) => voice.id === voiceId)) throw new Error("Unknown voice");
  ctx.db.patchCompanion(companionId, { voice: voiceId });
  for (const state of ctx.terminals()) {
    if (state.companionId !== companionId) continue;
    if (preview) ctx.edge.previewVoice(state.id, voiceId);
    if (state.drawer === "voice") await renderVoice(ctx, state);
  }
  ctx.notify();
}
