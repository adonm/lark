// Voice turns, device menus, listening policy, story reader and companion
// state. The router is the only place that knows how edge events map to a
// companion; HTTP, Delta Chat and the CLI all call the same methods here.
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type {
  EdgeCaptureDone,
  EdgeCommit,
  EdgeEventMap,
  EdgePlayback,
  EdgePointer,
  EdgeSpeculate,
  EdgeTerminal,
  EdgeTelemetry,
  Screen,
  ScreenChoice,
  ScreenItem,
  TuneNote,
} from "./edge_client.ts";
import type { SongMatch } from "./song_id.ts";
import type { Companion, LarkDb, Message, StoryRow } from "./db.ts";
import {
  evalPassage,
  flagsMap,
  flagsSlice,
  matchChoice,
  parseTwee,
  type Story,
  type StoryValue,
} from "./stories.ts";
import type { DcPeerTurn } from "./deltachat.ts";
import type { SpeculationHandle, TurnDelta, TurnResult } from "./opencode.ts";
import type { PortraitManager } from "./portraits.ts";
import { drain, errMessage } from "./util.ts";
import { nameMatches } from "./matching.ts";
import { DeltaCapture, SpeechStream, sanitizeSpeech } from "./speech.ts";

export { nameMatches, sanitizeSpeech };

export type { TurnDelta, TurnResult };
export type TurnHandle = SpeculationHandle;

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

export const LISTEN_WINDOW_MS = 30_000;
export const FOLLOW_UP_GRACE_MS = 10_000;
export const REPLY_CONTEXT_MS = 60_000;
export const NOT_CAUGHT_LINE = "I didn't catch that.";

export interface TurnBrain {
  createSession(companion: { id: string; name: string; personality: string }): Promise<string>;
  /** False when a stored session disappeared with an old OpenCode store. */
  sessionExists(sessionID: string): Promise<boolean>;
  runTurn(input: { sessionID: string; text: string }): AsyncGenerator<TurnDelta, TurnResult>;
  startSpeculate(input: { sessionID: string; text: string }, onDelta?: (delta: string) => void): TurnHandle;
}

export interface RouterEdge {
  on<K extends keyof EdgeEventMap>(name: K, listener: (event: EdgeEventMap[K]) => void): () => void;
  render(terminalId: string, screen: Screen): Promise<void>;
  speakStart(terminalId: string, streamId: number, voice: string): Promise<void>;
  speakChunk(terminalId: string, streamId: number, text: string): void;
  speakEnd(terminalId: string, streamId: number): void;
  stopAudio(terminalId: string): Promise<void>;
  chime(terminalId: string, kind: string): void;
  power(terminalId: string, brightness: number): void;
  volume(terminalId: string, level: number): void;
  recStart(
    terminalId: string,
    options?: { listenWindow?: boolean; windowS?: number; followUpGraceMs?: number; companionName?: string },
  ): Promise<void>;
  recCancel(terminalId: string): Promise<void>;
  previewVoice(terminalId: string, voice: string): void;
  metronome(terminalId: string, bpm: number, beats: number, durationS: number): Promise<number>;
  playTune(terminalId: string, tempo: number, style: string, notes: TuneNote[]): Promise<number>;
  captureStart(terminalId: string, outPath: string, seconds: number): Promise<void>;
  captureStop(terminalId: string): Promise<void>;
}

export interface RouterDc {
  sendText(accountId: number, chatId: number, text: string): Promise<void>;
  listPairedContacts(companionId: string): Promise<Array<{ id: number; name: string; address: string }>>;
  unpairContact(companionId: string, contactId: number): Promise<void>;
  pairInvite(companionId: string): Promise<string>;
  accountForCompanion(companionId: string): number;
}

interface StoryRun {
  id: string;
  story: Story;
  passage: number;
  flags: StoryValue[];
  title: string;
  body: string;
  choices: Array<{ label: string; target: number }>;
  page: number;
}

interface ReplyContext {
  accountId: number;
  chatId: number;
  contact: string;
  /** Lowercased Delta Chat address; scopes history to this conversation. */
  peer: string;
  armedUntil: number;
}

interface TerminalState {
  id: string;
  companionId: string;
  info: EdgeTerminal;
  telemetry?: EdgeTelemetry;
  listening: boolean;
  listeningSince: number;
  page: number;
  drawer: string;
  story?: StoryRun;
  busy: boolean;
  speaking: boolean;
  caption: string;
  audio?: "metronome" | "tune" | "identify";
  audioStreamId?: number;
  reply?: ReplyContext;
  previousBrightness: number;
  listenTimer?: ReturnType<typeof setTimeout>;
  speculation?: { transcript: string; stream: DeltaCapture; handle: TurnHandle };
  turn?: AbortController;
}

export interface RemoteTerminal {
  id: string;
  name: string;
  online: boolean;
  volume: number;
  battery_pct?: number;
  charging?: boolean;
}

export interface RemoteSnapshot {
  companion: {
    id: string;
    name: string;
    form: string;
    voice: string;
    listening: boolean;
    live_state: string;
    portrait?: string;
  };
  terminals: RemoteTerminal[];
  stories: Array<{ name: string; title: string; passages: number; progress: number; passage?: string }>;
  voices: Array<{ id: string; name: string }>;
}

export interface StorySummary {
  id: string;
  title: string;
  passages: number;
  progress: number;
  passage?: string;
}

export interface RouterOptions {
  db: LarkDb;
  edge: RouterEdge;
  brain: TurnBrain;
  dataDir: string;
  dc?: RouterDc;
  portraits?: Pick<PortraitManager, "request" | "dirFor" | "available">;
  identifySong?: (wavPath: string) => Promise<SongMatch>;
  now?: () => number;
  log?: (line: string) => void;
}

function statusOf(state: Pick<TerminalState, "speaking" | "busy" | "listening">): string {
  if (state.speaking) return "speaking";
  if (state.busy) return "thinking";
  if (state.listening) return "listening";
  return "idle";
}

export class Router {
  readonly #db: LarkDb;
  readonly #edge: RouterEdge;
  readonly #brain: TurnBrain;
  readonly #dc: RouterDc | undefined;
  readonly #portraits: RouterOptions["portraits"];
  readonly #identify: RouterOptions["identifySong"];
  readonly #now: () => number;
  readonly #log: (line: string) => void;
  readonly #terminals = new Map<string, TerminalState>();
  readonly #models = new Map<string, string>();
  // Portrait data URLs and parsed stories change only when their source
  // changes, so remote polling must not re-read or re-parse them every tick.
  readonly #portraitCache = new Map<string, string | null>();
  readonly #storyCache = new Map<string, { updatedAt: number; story: Story }>();
  #streamSeq = 0;
  #closed = false;

  constructor(options: RouterOptions) {
    this.#db = options.db;
    this.#edge = options.edge;
    this.#brain = options.brain;
    this.#dc = options.dc;
    this.#portraits = options.portraits;
    this.#identify = options.identifySong;
    this.#now = options.now ?? (() => Date.now());
    this.#log = options.log ?? (() => undefined);
    this.#bind();
  }

  #bind(): void {
    this.#edge.on("ready", (event) => {
      for (const [name, state] of Object.entries(event.models)) this.#models.set(name, state.state);
    });
    this.#edge.on("terminal.connected", (event) => void this.#connected(event));
    this.#edge.on("terminal.disconnected", (event) => void this.#disconnected(event.terminal_id));
    this.#edge.on("terminal.telemetry", (event) => this.#telemetry(event));
    this.#edge.on("pointer.action", (event) => {
      void this.#pointer(event).catch((error: unknown) => this.#log(`pointer: ${String(error)}`));
    });
    this.#edge.on("turn.speculate", (event) => {
      void this.#speculate(event).catch((error: unknown) => this.#log(`speculate: ${String(error)}`));
    });
    this.#edge.on("turn.commit", (event) => {
      void this.#commit(event).catch((error: unknown) => this.#log(`commit: ${String(error)}`));
    });
    this.#edge.on("turn.cancel", (event) => void this.#cancel(event.terminal_id, event.reason));
    this.#edge.on("turn.not_caught", (event) => void this.speakPhrase(event.terminal_id, NOT_CAUGHT_LINE));
    this.#edge.on("playback.done", (event) => void this.#playbackDone(event));
  }

  get models(): Map<string, string> {
    return new Map(this.#models);
  }

  async close(): Promise<void> {
    this.#closed = true;
    for (const terminal of this.#terminals.values()) {
      if (terminal.listenTimer !== undefined) clearTimeout(terminal.listenTimer);
      terminal.speculation?.handle.abort();
      terminal.turn?.abort();
    }
    this.#terminals.clear();
  }

  // ---- public state -------------------------------------------------------

  defaultCompanion(): Companion | undefined {
    return this.#db.listCompanions()[0];
  }

  companionForTerminal(terminalId: string): Companion | undefined {
    const state = this.#terminals.get(terminalId);
    if (state !== undefined) return this.#db.getCompanion(state.companionId);
    const row = this.#db.getTerminal(terminalId);
    if (row?.companionId != null) return this.#db.getCompanion(row.companionId);
    return this.defaultCompanion();
  }

  terminalForCompanion(companionId: string): string | undefined {
    for (const state of this.#terminals.values()) {
      if (state.companionId === companionId) return state.id;
    }
    return undefined;
  }

  onlineTerminals(companionId?: string): TerminalState[] {
    return [...this.#terminals.values()].filter(
      (state) => companionId === undefined || state.companionId === companionId,
    );
  }

  liveState(companionId: string): string {
    const state = this.onlineTerminals(companionId)[0];
    return state === undefined ? "offline" : statusOf(state);
  }

  remoteTerminals(companionId: string): RemoteTerminal[] {
    const out: RemoteTerminal[] = [];
    const seen = new Set<string>();
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      seen.add(state.id);
      out.push({
        id: state.id,
        name: state.id,
        online: true,
        volume: this.#db.getTerminalVolume(state.id),
        ...(state.telemetry === undefined
          ? {}
          : { battery_pct: state.telemetry.battery_pct, charging: state.telemetry.charging }),
      });
    }
    for (const row of this.#db.listTerminals()) {
      if (row.companionId !== companionId || seen.has(row.id)) continue;
      out.push({ id: row.id, name: row.id, online: false, volume: this.#db.getTerminalVolume(row.id) });
    }
    out.sort((a, b) => a.id.localeCompare(b.id));
    return out;
  }

  storySummaries(companionId: string): StorySummary[] {
    return this.#db.listStories().map((row) => {
      const progress = this.#db.getStoryProgress(companionId, row.id);
      let percent = 0;
      if (progress !== undefined) {
        percent = 10;
        try {
          const story = this.#parseStory(row);
          const index = story.byName.get(progress.passage);
          if (index !== undefined) {
            const passage = story.passages[index];
            percent = passage?.end === true ? 100 : Math.max(5, Math.min(95, ((index + 1) * 100) / story.passages.length));
          }
        } catch {
          percent = 10;
        }
        percent = Math.round(percent);
      }
      return {
        id: row.id,
        title: row.title,
        passages: row.passages,
        progress: percent,
        ...(progress === undefined ? {} : { passage: progress.passage }),
      };
    });
  }

  /** Parse each Twee once per updated_at; polling only touches the cache. */
  #parseStory(row: StoryRow): Story {
    const cached = this.#storyCache.get(row.id);
    if (cached !== undefined && cached.updatedAt === row.updatedAt) return cached.story;
    const story = parseTwee(row.twee);
    this.#storyCache.set(row.id, { updatedAt: row.updatedAt, story });
    return story;
  }

  /** Base64 sheets are expensive; the portrait id is their cache key. */
  #portraitData(companion: Companion | undefined): string | undefined {
    if (companion === undefined || companion.portraitId === "") return undefined;
    const id = companion.portraitId;
    if (this.#portraitCache.has(id)) return this.#portraitCache.get(id) ?? undefined;
    let data: string | null = null;
    const dir = this.#portraits?.dirFor(id);
    if (dir !== undefined) {
      try {
        data = `data:image/png;base64,${readFileSync(join(dir, "sheet.png")).toString("base64")}`;
      } catch {
        data = null;
      }
    }
    this.#portraitCache.set(id, data);
    return data ?? undefined;
  }

  snapshot(companionId: string, options: { includePortrait?: boolean } = {}): RemoteSnapshot {
    const companion = this.#db.getCompanion(companionId) ?? this.defaultCompanion();
    if (companion === undefined) {
      return {
        companion: { id: companionId, name: "Lark", form: "", voice: "af_heart", listening: false, live_state: "offline" },
        terminals: [],
        stories: [],
        voices: VOICES.map((voice) => ({ ...voice })),
      };
    }
    const portrait = options.includePortrait === false ? undefined : this.#portraitData(companion);
    return {
      companion: {
        id: companion.id,
        name: companion.name,
        form: companion.form,
        voice: companion.voice === "" ? "af_heart" : companion.voice,
        listening: companion.listening,
        live_state: this.liveState(companion.id),
        ...(portrait === undefined ? {} : { portrait }),
      },
      terminals: this.remoteTerminals(companion.id),
      stories: this.storySummaries(companion.id).map((story) => ({
        name: story.id,
        title: story.title,
        passages: story.passages,
        progress: story.progress,
        ...(story.passage === undefined ? {} : { passage: story.passage }),
      })),
      voices: VOICES.map((voice) => ({ ...voice })),
    };
  }

  diagnostics(): Record<string, unknown> {
    return {
      companions: this.#db.listCompanions().length,
      terminals: [...this.#terminals.values()].map((state) => ({
        id: state.id,
        companion_id: state.companionId,
        listening: state.listening,
        busy: state.busy,
        speaking: state.speaking,
        drawer: state.drawer,
        story: state.story?.story.title,
      })),
      models: Object.fromEntries(this.#models),
      timers: this.#db.listTimers().length,
      stories: this.#db.listStories().length,
    };
  }

  // ---- terminal lifecycle -------------------------------------------------

  #terminalCompanion(terminalId: string): string | undefined {
    const row = this.#db.getTerminal(terminalId);
    if (row?.companionId != null && this.#db.getCompanion(row.companionId) !== undefined) return row.companionId;
    return this.defaultCompanion()?.id;
  }

  async #connected(info: EdgeTerminal): Promise<void> {
    const companionId = this.#terminalCompanion(info.terminal_id);
    if (companionId === undefined) return;
    const state: TerminalState = {
      id: info.terminal_id,
      companionId,
      info,
      listening: false,
      listeningSince: 0,
      page: 0,
      drawer: "",
      busy: false,
      speaking: false,
      caption: "",
      previousBrightness: 100,
    };
    this.#terminals.set(info.terminal_id, state);
    this.#db.upsertTerminal({
      id: info.terminal_id,
      kind: info.board === "" ? "terminal" : info.board,
      companionId,
      ip: info.ip,
      port: 0,
      lastSeen: this.#now(),
      createdAt: new Date(this.#now()).toISOString(),
    });
    this.#db.setTerminalVolume(info.terminal_id, this.#db.getTerminalVolume(info.terminal_id));
    const companion = this.#db.getCompanion(companionId);
    if (companion !== undefined && companion.listening) await this.#armListening(state);
    else await this.#renderHome(state, "idle");
  }

  async #disconnected(terminalId: string): Promise<void> {
    const state = this.#terminals.get(terminalId);
    if (state !== undefined) {
      if (state.listenTimer !== undefined) clearTimeout(state.listenTimer);
      state.speculation?.handle.abort();
      state.turn?.abort();
      this.#db.upsertTerminal({
        id: state.id,
        kind: state.info.board,
        companionId: state.companionId,
        ip: state.info.ip,
        port: 0,
        lastSeen: this.#now(),
        createdAt: new Date(this.#now()).toISOString(),
      });
    }
    this.#terminals.delete(terminalId);
  }

  #telemetry(event: EdgeTelemetry): void {
    const state = this.#terminals.get(event.terminal_id);
    if (state !== undefined) state.telemetry = event;
  }

  #portraitDir(companion: Companion | undefined): string | undefined {
    if (companion === undefined || companion.portraitId === "") return undefined;
    return this.#portraits?.dirFor(companion.portraitId);
  }

  async #renderHome(state: TerminalState, status: string, caption = state.caption): Promise<void> {
    const companion = this.#db.getCompanion(state.companionId);
    if (companion === undefined) return;
    state.caption = caption;
    await this.#edge.render(state.id, {
      kind: "home",
      portrait_dir: this.#portraitDir(companion),
      companion_name: companion.name,
      companion_form: companion.form,
      status,
      caption,
      listen_window: state.listening,
    });
  }

  async refreshCompanion(companionId: string): Promise<void> {
    // A completed portrait replaces the sheet behind a new id; drop the cached
    // data URLs so a same-id rewrite is also picked up.
    this.#portraitCache.clear();
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      if (state.drawer === "") await this.#renderHome(state, statusOf(state));
    }
  }

  // ---- pointer and menus --------------------------------------------------

  async #pointer(event: EdgePointer): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined) return;
    const companion = this.#db.getCompanion(state.companionId);
    if (companion === undefined) return;
    const hit = event.action;
    const value = event.value;

    if (hit === "listen") {
      // The edge opens the tap-to-talk capture itself; re-issuing rec_start
      // here would cancel the window while the speaker is already talking.
      await this.#armListening(state, false);
      this.#edge.chime(state.id, "tap");
      return;
    }
    if (hit === "listen-stop" || hit === "stop") {
      const identifying = state.audio === "identify";
      state.listening = false;
      state.audio = undefined;
      state.audioStreamId = undefined;
      if (identifying) await this.#edge.captureStop(state.id).catch(() => undefined);
      await this.#edge.stopAudio(state.id);
      await this.#renderHome(state, "idle");
      return;
    }
    if (hit === "menu") {
      state.drawer = "menu";
      state.page = 0;
      await this.#renderDrawer(state);
      return;
    }
    if (hit === "menu-back" || hit === "pair-close") {
      if (state.drawer === "pair") this.#edge.power(state.id, state.previousBrightness);
      if (state.drawer === "story-reader") {
        state.story = undefined;
        state.drawer = "stories";
      } else if (state.drawer === "menu") {
        state.drawer = "";
        await this.#renderHome(state, statusOf(state));
        return;
      } else if (["volume", "pair", "unpair", "stats"].includes(state.drawer)) {
        state.drawer = "more";
        state.page = 0;
      } else {
        state.drawer = "menu";
        state.page = 0;
      }
      await this.#renderDrawer(state);
      return;
    }
    if (hit === "menu-prev" || hit === "menu-next" || hit === "menu-page") {
      const parsed = Number.parseInt(value === "" ? hit.replace("menu-", "") : value, 10);
      state.page = Number.isFinite(parsed) ? Math.max(0, parsed) : Math.max(0, state.page - 1);
      if (state.story !== undefined && state.drawer === "story-reader") {
        state.story.page = state.page;
        await this.#renderStory(state);
        await this.#saveStoryProgress(state);
      } else {
        await this.#renderDrawer(state);
      }
      return;
    }
    if (hit.startsWith("choice-")) {
      const index = Number.parseInt(hit.slice("choice-".length), 10);
      if (Number.isFinite(index)) await this.#chooseStory(state, index);
      return;
    }
    if (hit === "vol-up" || hit === "vol-down") {
      const delta = hit === "vol-up" ? 10 : -10;
      const level = Math.max(0, Math.min(100, this.#db.getTerminalVolume(state.id) + delta));
      this.#db.setTerminalVolume(state.id, level);
      this.#edge.volume(state.id, level);
      await this.#renderVolume(state, level);
      return;
    }
    if (state.drawer === "volume" || hit.startsWith("volume")) {
      await this.#renderVolume(state, this.#db.getTerminalVolume(state.id));
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
      await this.#renderDrawer(state);
      return;
    }
    if (state.drawer === "voice") {
      const voiceId = value !== "" ? value : hit;
      await this.setVoice(companion.id, voiceId, true);
      await this.#renderVoice(state);
      return;
    }
    if (state.drawer === "stories") {
      const story = this.#db.listStories().find((row) => row.id === hit || row.id === value);
      if (story !== undefined) {
        await this.openStory(companion.id, story.id, false);
        return;
      }
    }
    if (state.drawer === "unpair" && (hit.startsWith("unpair-") || value !== "")) {
      const contactId = Number.parseInt(value !== "" ? value : hit.replace("unpair-", ""), 10);
      if (Number.isFinite(contactId)) await this.#unpair(state, contactId);
      return;
    }
    if (state.drawer === "memories" || state.drawer === "talk") {
      await this.#renderDetail(state, hit);
      return;
    }
    if (actionValue === "companion" || actionValue === "dismiss" || actionValue === "close-card") {
      state.drawer = "";
      await this.#renderHome(state, statusOf(state));
    }
  }

  async #renderDrawer(state: TerminalState): Promise<void> {
    const companion = this.#db.getCompanion(state.companionId);
    if (companion === undefined) return;
    switch (state.drawer) {
      case "menu":
        await this.#edge.render(state.id, { kind: "menu", page: state.page });
        return;
      case "more":
        await this.#edge.render(state.id, {
          kind: "list",
          title: "More",
          page: state.page,
          items: [
            { id: "volume", label: "Volume", detail: `${this.#db.getTerminalVolume(state.id)}%`, icon: "volume" },
            { id: "pair", label: "Pair", detail: "Scan my code", icon: "pair" },
            { id: "unpair", label: "Unpair", detail: "Remove a contact", icon: "unpair" },
            { id: "stats", label: "Stats", detail: "Device and brain", icon: "stats" },
          ],
        });
        return;
      case "volume":
        await this.#renderVolume(state, this.#db.getTerminalVolume(state.id));
        return;
      case "voice":
        await this.#renderVoice(state);
        return;
      case "stats":
        await this.#edge.render(state.id, {
          kind: "stats",
          rows: this.#statsRows(state),
        });
        return;
      case "pair": {
        if (this.#dc === undefined) {
          await this.#renderDetail(state, "pair", "Pairing unavailable", "Delta Chat is not running.");
          return;
        }
        state.previousBrightness = 100;
        this.#edge.power(state.id, 100);
        try {
          const invite = await this.#dc.pairInvite(companion.id);
          await this.#edge.render(state.id, {
            kind: "pair_qr",
            companion_name: companion.name,
            invite,
          });
        } catch (error) {
          await this.#renderDetail(state, "pair", "Pairing unavailable", errMessage(error));
        }
        return;
      }
      case "unpair": {
        const contacts = this.#dc === undefined ? [] : await this.#dc.listPairedContacts(companion.id);
        await this.#edge.render(state.id, {
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
        await this.#edge.render(state.id, {
          kind: "list",
          title: "Talk to…",
          page: state.page,
          items: this.#db
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
        await this.#edge.render(state.id, {
          kind: "list",
          title: "Memories",
          page: state.page,
          items: this.#memories(companion.id),
        });
        return;
      case "stories":
        await this.#edge.render(state.id, {
          kind: "list",
          title: "Stories",
          page: state.page,
          items: this.storySummaries(companion.id).map((story) => ({
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
        await this.#edge.render(state.id, { kind: "menu", page: 0 });
    }
  }

  async #renderVolume(state: TerminalState, level: number): Promise<void> {
    this.#edge.volume(state.id, level);
    await this.#edge.render(state.id, { kind: "volume", level, page: state.page });
  }

  async #renderVoice(state: TerminalState): Promise<void> {
    const companion = this.#db.getCompanion(state.companionId);
    await this.#edge.render(state.id, {
      kind: "voice",
      current: companion?.voice ?? "af_heart",
      page: state.page,
      voices: VOICES.map((voice) => ({ id: voice.id, label: voice.name })),
    });
  }

  #memories(companionId: string): ScreenItem[] {
    const rows = [...this.#db.listMemory("shared"), ...this.#db.listMemory(`companion:${companionId}`)];
    return rows.slice(0, 24).map((row) => ({
      id: `memory-${row.scope}-${row.key}`,
      label: row.key,
      detail: typeof row.value === "string" ? row.value.slice(0, 80) : JSON.stringify(row.value).slice(0, 80),
      icon: "memories",
    }));
  }

  #statsRows(state: TerminalState): Array<{ title: string; text: string }> {
    const companion = this.#db.getCompanion(state.companionId);
    const rows = [
      { title: "Companion", text: companion?.name ?? "—" },
      { title: "Voice", text: companion?.voice === "" ? "Heart" : companion?.voice ?? "—" },
      { title: "Terminal", text: state.info.board === "" ? state.id : `${state.info.board} · ${state.info.firmware}` },
      { title: "Volume", text: `${this.#db.getTerminalVolume(state.id)}%` },
      { title: "Timers", text: String(this.#db.countTimers(state.companionId)) },
      { title: "Stories", text: String(this.#db.listStories().length) },
    ];
    if (state.telemetry !== undefined) {
      rows.push(
        { title: "Battery", text: `${state.telemetry.battery_pct}%${state.telemetry.charging ? " charging" : ""}` },
        { title: "Wi-Fi", text: `${state.telemetry.wifi_rssi} dBm` },
      );
    }
    return rows;
  }

  async #renderDetail(state: TerminalState, id: string, heading?: string, body?: string): Promise<void> {
    let title = heading ?? "Details";
    let text = body ?? "";
    if (id.startsWith("memory-")) {
      const row = [...this.#db.listMemory("shared"), ...this.#db.listMemory(`companion:${state.companionId}`)].find(
        (memory) => `memory-${memory.scope}-${memory.key}` === id,
      );
      if (row !== undefined) {
        title = row.key;
        text = typeof row.value === "string" ? row.value : JSON.stringify(row.value, null, 2);
      }
    } else if (id.startsWith("talk-")) {
      const peer = this.#db.getCompanion(id.slice("talk-".length));
      if (peer !== undefined) {
        title = peer.name;
        text = peer.form === "" ? "Say hello on the device." : peer.form;
      }
    }
    await this.#edge.render(state.id, { kind: "detail", title, heading: title, body: text });
  }

  async #unpair(state: TerminalState, contactId: number): Promise<void> {
    if (this.#dc === undefined) return;
    try {
      await this.#dc.unpairContact(state.companionId, contactId);
      this.#edge.chime(state.id, "tap");
    } catch (error) {
      this.#log(`unpair: ${errMessage(error)}`);
    }
    state.drawer = "unpair";
    await this.#renderDrawer(state);
  }

  // ---- stories ------------------------------------------------------------

  async openStory(companionId: string, storyId: string, restart: boolean): Promise<void> {
    const row = this.#db.getStory(storyId);
    if (row === undefined) throw new Error(`story ${storyId} not found`);
    const story = this.#parseStory(row);
    let passage = story.start;
    let flags = flagsSlice(story);
    let page = 0;
    const progress = restart ? undefined : this.#db.getStoryProgress(companionId, storyId);
    if (progress !== undefined) {
      const index = story.byName.get(progress.passage);
      if (index !== undefined) {
        passage = index;
        flags = flagsSlice(story, progress.flags);
        page = Math.max(0, progress.page);
      }
    }
    if (restart) this.#db.deleteStoryProgress(companionId, storyId);
    const evaluated = evalPassage(story, passage, flags);
    const run: StoryRun = {
      id: row.id,
      story,
      passage,
      flags,
      title: story.title,
      body: evaluated.text,
      choices: evaluated.choices,
      page,
    };
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      state.story = run;
      state.drawer = "story-reader";
      state.page = page;
      await this.#edge.stopAudio(state.id);
      await this.#renderStory(state);
      await this.#saveStoryProgress(state);
    }
  }

  async restartStory(companionId: string, storyId: string): Promise<void> {
    await this.openStory(companionId, storyId, true);
  }

  async #renderStory(state: TerminalState): Promise<void> {
    const run = state.story;
    if (run === undefined) return;
    const companion = this.#db.getCompanion(state.companionId);
    const choices: ScreenChoice[] = run.choices.map((choice, index) => ({
      id: `choice-${index}`,
      label: choice.label,
      value: String(choice.target),
    }));
    await this.#edge.render(state.id, {
      kind: "reader",
      portrait_dir: this.#portraitDir(companion),
      title: run.title,
      body: run.body,
      choices,
      page: run.page,
    });
  }

  async #chooseStory(state: TerminalState, index: number): Promise<void> {
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
    await this.#renderStory(state);
    await this.#saveStoryProgress(state);
  }

  async #saveStoryProgress(state: TerminalState): Promise<void> {
    const run = state.story;
    if (run === undefined) return;
    const passage = run.story.passages[run.passage];
    this.#db.saveStoryProgress({
      companionId: state.companionId,
      storyId: run.id,
      passage: passage?.name ?? "",
      page: run.page,
      flags: flagsMap(run.story, run.flags),
      updatedAt: this.#now(),
    });
  }

  async #spokenStoryChoice(state: TerminalState, transcript: string): Promise<boolean> {
    const run = state.story;
    if (run === undefined || run.choices.length === 0) return false;
    const result = matchChoice(transcript, run.choices);
    if (!result.matched) return false;
    await this.#chooseStory(state, result.index);
    return true;
  }

  // ---- voice turns --------------------------------------------------------

  async #sessionFor(companionId: string): Promise<string> {
    const stored = this.#db.getSession(companionId);
    if (stored !== undefined) {
      if (await this.#brain.sessionExists(stored.sessionId)) return stored.sessionId;
      // The session table survives a v1 cutover, but its ids belong to the old
      // OpenCode store. Drop the stale binding and start a fresh session.
      this.#db.deleteSession(companionId);
    }
    const companion = this.#db.getCompanion(companionId);
    if (companion === undefined) return Promise.reject(new Error(`companion ${companionId} not found`));
    return this.#brain.createSession({ id: companion.id, name: companion.name, personality: companion.personality });
  }

  async #speculate(event: EdgeSpeculate): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined || state.busy || state.drawer === "story-reader" || event.transcript.trim() === "") return;
    state.speculation?.handle.abort();
    const sessionID = await this.#sessionFor(state.companionId);
    const capture = new DeltaCapture();
    const handle = this.#brain.startSpeculate({ sessionID, text: event.transcript.trim() }, (delta) => capture.push(delta));
    state.speculation = { transcript: event.transcript.trim(), stream: capture, handle };
    void handle.result.catch(() => undefined);
  }

  async #commit(event: EdgeCommit): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined || this.#closed || state.busy) return;
    const companion = this.#db.getCompanion(state.companionId);
    if (companion === undefined) return;
    state.listening = false;

    // A silent story reader uses spoken choice matching and never synthesizes
    // the passage itself.
    if (state.drawer === "story-reader") {
      const advanced = await this.#spokenStoryChoice(state, event.transcript);
      if (advanced) return;
      if (/\b(stop|menu|home)\b/i.test(event.transcript)) {
        state.story = undefined;
        state.drawer = "";
        await this.#renderHome(state, "idle");
      }
      return;
    }

    if (event.listen_window && companion.listening) {
      const elapsed = this.#now() - state.listeningSince;
      if (elapsed > FOLLOW_UP_GRACE_MS && !nameMatches(event.transcript, companion.name)) {
        await this.#armListening(state);
        return;
      }
    }

    const transcript = event.transcript.trim();
    if (transcript === "" || /^\[silent\]$/i.test(transcript)) {
      await this.#armListening(state);
      return;
    }

    state.busy = true;
    await this.#renderHome(state, "thinking");
    this.#edge.chime(state.id, "think");

    const speculation = state.speculation;
    state.speculation = undefined;
    const matching = speculation !== undefined && speculation.transcript === transcript;
    if (speculation !== undefined && !matching) speculation.handle.abort();

    const controller = new AbortController();
    state.turn = controller;
    const speech = new SpeechStream(this.#edge, state.id, companion.voice === "" ? "af_heart" : companion.voice, ++this.#streamSeq);
    const capture = matching ? speculation.stream : new DeltaCapture();
    if (matching) speculation.handle.promote();
    // Both paths stream through the same sentence splitter: promotion flushes
    // the speculative text already produced, a fresh turn forwards live deltas.
    capture.commit(speech);

    let full = "";
    try {
      if (matching) {
        const result = await speculation.handle.result;
        full = result.text;
      } else {
        const sessionID = await this.#sessionFor(state.companionId);
        const result = await drain(this.#brain.runTurn({ sessionID, text: transcript }), (delta) => capture.push(delta.text));
        full = result.text;
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        this.#log(`turn: ${errMessage(error)}`);
      }
    } finally {
      state.turn = undefined;
      state.busy = false;
    }
    await speech.end();

    const spoken = sanitizeSpeech(full);
    this.#db.appendMessage({
      id: `turn:${state.id}:${this.#now()}:user`,
      companionId: state.companionId,
      role: "user",
      sender: "Someone",
      text: transcript,
      source: "voice",
      channel: "in_person",
      cardTitle: "",
      tsMs: this.#now(),
      peer: "",
    });
    if (spoken === "") {
      await this.#armListening(state);
      return;
    }

    this.#db.appendMessage({
      id: `turn:${state.id}:${this.#now()}:reply`,
      companionId: state.companionId,
      role: "assistant",
      sender: companion.name,
      text: full,
      source: "voice",
      channel: "in_person",
      cardTitle: "",
      tsMs: this.#now(),
      peer: "",
    });
    state.caption = spoken.slice(0, 120);
    state.speaking = true;
    await this.#renderHome(state, "speaking", state.caption);

    if (state.reply !== undefined && this.#now() <= state.reply.armedUntil) {
      await this.#routeReply(state, full);
    }
  }

  async #routeReply(state: TerminalState, text: string): Promise<void> {
    const reply = state.reply;
    if (reply === undefined || this.#dc === undefined) return;
    try {
      await this.#dc.sendText(reply.accountId, reply.chatId, text);
      this.#db.appendMessage({
        id: `dc:${reply.accountId}:${reply.chatId}:${this.#now()}:reply`,
        companionId: state.companionId,
        role: "assistant",
        sender: this.#db.getCompanion(state.companionId)?.name ?? "Companion",
        text,
        source: "deltachat",
        channel: "deltachat",
        cardTitle: "",
        tsMs: this.#now(),
        peer: reply.peer,
      });
    } catch (error) {
      this.#log(`reply routing: ${errMessage(error)}`);
    }
  }

  async #playbackDone(event: EdgePlayback): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined) return;
    // Identify has no playback stream; a stale stop event must not close the
    // listening screen. A replaced metronome/tune stream is likewise ignored.
    if (state.audio === "identify") return;
    if (state.audioStreamId !== undefined && event.stream_id !== state.audioStreamId) return;
    state.audio = undefined;
    state.audioStreamId = undefined;
    state.speaking = false;
    if (event.aborted) {
      state.busy = false;
      await this.#renderHome(state, "idle");
      return;
    }
    const companion = this.#db.getCompanion(state.companionId);
    if (state.reply !== undefined) {
      state.reply.armedUntil = this.#now() + REPLY_CONTEXT_MS;
    }
    if (companion?.listening === true) await this.#armListening(state);
    else await this.#renderHome(state, "idle");
  }

  async #cancel(terminalId: string, reason: string): Promise<void> {
    const state = this.#terminals.get(terminalId);
    if (state === undefined) return;
    state.speculation?.handle.abort(new Error(reason));
    state.speculation = undefined;
    state.turn?.abort();
    state.turn = undefined;
    state.busy = false;
    state.speaking = false;
    state.listening = false;
    await this.#renderHome(state, "idle");
  }

  async #armListening(state: TerminalState, startRec = true): Promise<void> {
    if (this.#closed) return;
    const companion = this.#db.getCompanion(state.companionId);
    if (state.listenTimer !== undefined) clearTimeout(state.listenTimer);
    state.listening = true;
    state.listeningSince = this.#now();
    state.drawer = "";
    state.story = undefined;
    await this.#renderHome(state, "listening");
    if (startRec) {
      try {
        await this.#edge.recStart(state.id, {
          listenWindow: true,
          windowS: Math.round(LISTEN_WINDOW_MS / 1000),
          followUpGraceMs: FOLLOW_UP_GRACE_MS,
          companionName: companion?.name ?? "",
        });
      } catch (error) {
        this.#log(`listen arm: ${errMessage(error)}`);
      }
    }
    state.listenTimer = setTimeout(() => {
      state.listenTimer = undefined;
      if (state.listening && !state.busy && !state.speaking) {
        state.listening = false;
        void this.#renderHome(state, "idle").catch(() => undefined);
      }
    }, LISTEN_WINDOW_MS + 2000);
    state.listenTimer.unref();
  }

  // ---- music and song identification --------------------------------------

  #audioTerminal(companionId: string): TerminalState {
    const state = this.onlineTerminals(companionId)[0];
    if (state === undefined) throw new Error("terminal is offline");
    return state;
  }

  async audioMetronome(companionId: string, bpm: number, beats: number, durationS: number): Promise<Record<string, unknown>> {
    const state = this.#audioTerminal(companionId);
    await this.#edge.stopAudio(state.id);
    const caption = `Metronome · ${bpm} BPM (${beats}/4)`;
    state.audio = "metronome";
    state.audioStreamId = undefined;
    state.speaking = true;
    state.listening = false;
    state.drawer = "";
    state.story = undefined;
    await this.#renderHome(state, "speaking", caption);
    const streamId = await this.#edge.metronome(state.id, bpm, beats, durationS);
    state.audioStreamId = streamId;
    return { terminal_id: state.id, caption, bpm, beats, duration_s: durationS, stream_id: streamId };
  }

  async audioTune(
    companionId: string,
    tempo: number,
    style: string,
    notes: TuneNote[],
    title?: string,
  ): Promise<Record<string, unknown>> {
    const state = this.#audioTerminal(companionId);
    await this.#edge.stopAudio(state.id);
    const caption = title === undefined || title === "" ? `Tune · ${style}` : `Tune · ${title} (${style})`;
    state.audio = "tune";
    state.audioStreamId = undefined;
    state.speaking = true;
    state.listening = false;
    state.drawer = "";
    state.story = undefined;
    await this.#renderHome(state, "speaking", caption);
    const streamId = await this.#edge.playTune(state.id, tempo, style, notes);
    state.audioStreamId = streamId;
    return { terminal_id: state.id, caption, tempo, style, notes: notes.length, stream_id: streamId };
  }

  async audioIdentify(companionId: string, seconds: number): Promise<SongMatch & Record<string, unknown>> {
    const state = this.#audioTerminal(companionId);
    if (this.#identify === undefined) throw new Error("song recognition is unavailable");
    const dir = mkdtempSync(join(tmpdir(), "lark-song-"));
    const wav = join(dir, "capture.wav");
    try {
      await this.#edge.stopAudio(state.id);
      state.audio = "identify";
      state.audioStreamId = undefined;
      state.speaking = false;
      state.listening = true;
      state.drawer = "";
      state.story = undefined;
      await this.#renderHome(state, "listening", "Listening to music...");
      const captured = new Promise<EdgeCaptureDone>((resolve, reject) => {
        let off: () => void = () => undefined;
        const timer = setTimeout(() => {
          off();
          reject(new Error("music capture timed out"));
        }, (seconds + 8) * 1000);
        off = this.#edge.on("capture.done", (event) => {
          if (event.terminal_id !== state.id || event.path !== wav) return;
          clearTimeout(timer);
          off();
          resolve(event);
        });
      });
      await this.#edge.captureStart(state.id, wav, seconds);
      const done = await captured;
      if (done.error !== undefined && done.error !== "") throw new Error(done.error);
      return { ...(await this.#identify(wav)), terminal_id: state.id, seconds };
    } finally {
      state.audio = undefined;
      state.audioStreamId = undefined;
      state.listening = false;
      await this.#renderHome(state, "idle", "").catch(() => undefined);
      rmSync(dir, { recursive: true, force: true });
    }
  }

  async audioStop(companionId: string): Promise<Record<string, unknown>> {
    const state = this.#audioTerminal(companionId);
    const stopped = state.audio !== undefined;
    if (state.audio === "identify") await this.#edge.captureStop(state.id).catch(() => undefined);
    state.audio = undefined;
    state.audioStreamId = undefined;
    state.listening = false;
    state.speaking = false;
    await this.#edge.stopAudio(state.id);
    await this.#renderHome(state, "idle", "");
    return { terminal_id: state.id, stopped };
  }

  // ---- shared commands ----------------------------------------------------

  async sayNow(companionId: string, text: string): Promise<void> {
    const state = this.onlineTerminals(companionId)[0];
    if (state === undefined) return;
    await this.speakPhrase(state.id, text);
  }

  async speakPhrase(terminalId: string, text: string): Promise<void> {
    const state = this.#terminals.get(terminalId);
    if (state === undefined) return;
    const companion = this.#db.getCompanion(state.companionId);
    const stream = new SpeechStream(this.#edge, terminalId, companion?.voice === "" ? "af_heart" : companion?.voice ?? "af_heart", ++this.#streamSeq);
    stream.push(text);
    await stream.end();
    state.speaking = true;
    state.caption = sanitizeSpeech(text).slice(0, 120);
    await this.#renderHome(state, "speaking", state.caption);
  }

  async setListening(companionId: string, listening: boolean): Promise<void> {
    this.#db.patchCompanion(companionId, { listening });
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      if (listening) await this.#armListening(state);
      else {
        state.listening = false;
        await this.#edge.recCancel(state.id);
        await this.#renderHome(state, "idle");
      }
    }
  }

  setVolume(terminalId: string, level: number): void {
    const clamped = Math.max(0, Math.min(100, Math.round(level)));
    this.#db.setTerminalVolume(terminalId, clamped);
    this.#edge.volume(terminalId, clamped);
  }

  async setVoice(companionId: string, voiceId: string, preview: boolean): Promise<void> {
    if (!VOICES.some((voice) => voice.id === voiceId)) throw new Error("Unknown voice");
    this.#db.patchCompanion(companionId, { voice: voiceId });
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      if (preview) this.#edge.previewVoice(state.id, voiceId);
      if (state.drawer === "voice") await this.#renderVoice(state);
    }
  }

  async notifyDcTurn(turn: DcPeerTurn & { accountId?: number; chatId?: number }): Promise<string | undefined> {
    const companion = this.#db.getCompanion(turn.companionId);
    if (companion === undefined) return undefined;
    const state = this.onlineTerminals(turn.companionId)[0];
    if (state !== undefined && !turn.private) {
      if (turn.accountId !== undefined && turn.chatId !== undefined) {
        state.reply = {
          accountId: turn.accountId,
          chatId: turn.chatId,
          contact: turn.sender,
          peer: turn.peer,
          armedUntil: 0,
        };
      }
      await this.speakPhrase(state.id, `Message from ${turn.sender}: ${turn.text}`);
    }
    try {
      const sessionID = await this.#sessionFor(turn.companionId);
      const result = await drain(this.#brain.runTurn({ sessionID, text: `${turn.sender} via Delta Chat: ${turn.text}` }));
      return sanitizeSpeech(result.text);
    } catch (error) {
      this.#log(`dc turn: ${errMessage(error)}`);
      return undefined;
    }
  }

  history(companionId: string, channel: "all" | "in_person" | "deltachat", contact: string, contactName: string, q: string, limit: number, cursor?: { beforeTs: number; beforeId: string }): { items: Array<Record<string, unknown>>; has_more: boolean; next_cursor?: string } {
    const page = this.#db.listRemoteMessages({
      companionId,
      channel,
      contact,
      contactName,
      q,
      limit,
      ...(cursor === undefined ? {} : { cursor }),
    });
    const companion = this.#db.getCompanion(companionId);
    const items = page.messages.map((message: Message) => ({
      id: message.id,
      role: message.role,
      speaker:
        message.role === "assistant" || String(message.role) === "companion"
          ? companion?.name ?? "Companion"
          : message.sender,
      channel: message.channel,
      text: message.text.trim(),
      ts_ms: message.tsMs,
    }));
    return {
      items,
      has_more: page.hasMore,
      ...(page.nextCursor === undefined
        ? {}
        : {
            next_cursor: Buffer.from(`${page.nextCursor.beforeTs}:${page.nextCursor.beforeId}`).toString("base64url"),
          }),
    };
  }
}
