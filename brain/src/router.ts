// Voice turns, device menus, listening policy, story reader and companion
// state. The router is the only place that knows how edge events map to a
// companion; HTTP, Delta Chat and the CLI all call the same methods here.
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type {
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
} from "./edge_client.ts";
import type { Companion, LarkDb, Message } from "./db.ts";
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
import type { PortraitManager } from "./portraits.ts";

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

export interface TurnDelta {
  type: "delta";
  text: string;
}

export interface TurnResult {
  sessionID: string;
  text: string;
  model: unknown;
  llmFirstMs: number;
  llmTotalMs: number;
}

export interface TurnHandle {
  sessionID: string;
  result: Promise<TurnResult>;
  promote(): void;
  abort(reason?: Error): void;
}

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
  filler(terminalId: string, voice: string): void;
  previewVoice(terminalId: string, voice: string): void;
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
  now?: () => number;
  log?: (line: string) => void;
}

function normalize(text: string): string {
  return text.toLowerCase().replace(/[^a-z0-9 ]+/g, " ").replace(/\s+/g, " ").trim();
}

function levenshtein(a: string, b: string): number {
  if (a === b) return 0;
  const rows = new Array<number>(b.length + 1);
  for (let j = 0; j <= b.length; j++) rows[j] = j;
  for (let i = 1; i <= a.length; i++) {
    let previous = rows[0] ?? 0;
    rows[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const current = rows[j] ?? 0;
      rows[j] = Math.min(
        (rows[j] ?? 0) + 1,
        (rows[j - 1] ?? 0) + 1,
        previous + (a[i - 1] === b[j - 1] ? 0 : 1),
      );
      previous = current;
    }
  }
  return rows[b.length] ?? a.length;
}

/** Fuzzy companion-name match in the first five words (edit distance ≤ 2). */
export function nameMatches(transcript: string, name: string): boolean {
  const target = normalize(name);
  if (target === "") return false;
  const words = normalize(transcript).split(" ").slice(0, 5);
  for (const word of words) {
    if (word === "") continue;
    if (word === target) return true;
    for (const part of target.split(" ")) {
      if (part !== "" && levenshtein(word, part) <= 2) return true;
    }
  }
  return false;
}

export function sanitizeSpeech(text: string): string {
  return text
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/[*_#`>]+/g, "")
    .replace(/\[silent\]/gi, "")
    .replace(/\s+/g, " ")
    .trim();
}

interface Sink {
  push(text: string): void;
}

/** Accumulates deltas until a turn is promoted, then forwards them in order. */
class DeltaCapture {
  readonly #buffer: string[] = [];
  #sink: Sink | undefined;

  get buffered(): string {
    return this.#buffer.join("");
  }

  push(text: string): void {
    if (this.#sink === undefined) {
      this.#buffer.push(text);
    } else {
      this.#sink.push(text);
    }
  }

  commit(sink: Sink): void {
    if (this.#sink !== undefined) return;
    this.#sink = sink;
    const pending = this.#buffer.join("");
    this.#buffer.length = 0;
    if (pending !== "") sink.push(pending);
  }
}

class SpeechStream implements Sink {
  readonly #edge: RouterEdge;
  readonly #terminalId: string;
  readonly #voice: string;
  readonly #streamId: number;
  #chain: Promise<void> = Promise.resolve();
  #started = false;
  #buffer = "";
  #closed = false;

  constructor(edge: RouterEdge, terminalId: string, voice: string, streamId: number) {
    this.#edge = edge;
    this.#terminalId = terminalId;
    this.#voice = voice;
    this.#streamId = streamId;
  }

  push(text: string): void {
    if (this.#closed) return;
    this.#buffer += text;
    for (;;) {
      const cut = this.#cut(this.#buffer);
      if (cut < 0) return;
      const chunk = this.#buffer.slice(0, cut).trim();
      this.#buffer = this.#buffer.slice(cut);
      if (chunk !== "") void this.#chunk(chunk);
    }
  }

  #cut(buffer: string): number {
    const sentence = /[.!?](?:["'’”)]*)\s/.exec(buffer);
    if (sentence !== null) {
      return (sentence.index ?? 0) + sentence[0].length;
    }
    if (buffer.length >= 140) {
      const clause = /[,;:]\s/.exec(buffer.slice(40));
      if (clause !== null) return 40 + (clause.index ?? 0) + clause[0].length;
      const space = buffer.lastIndexOf(" ");
      if (space > 60) return space + 1;
    }
    return -1;
  }

  #chunk(text: string): void {
    const clean = sanitizeSpeech(text);
    if (clean === "") return;
    this.#chain = this.#chain.then(async () => {
      if (!this.#started) {
        this.#started = true;
        await this.#edge.speakStart(this.#terminalId, this.#streamId, this.#voice);
      }
      this.#edge.speakChunk(this.#terminalId, this.#streamId, clean);
    });
  }

  async end(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    const rest = sanitizeSpeech(this.#buffer.trim());
    this.#buffer = "";
    if (rest !== "") await this.#chain.then(async () => {
      if (!this.#started) {
        this.#started = true;
        await this.#edge.speakStart(this.#terminalId, this.#streamId, this.#voice);
      }
      this.#edge.speakChunk(this.#terminalId, this.#streamId, rest);
    });
    await this.#chain;
    if (this.#started) this.#edge.speakEnd(this.#terminalId, this.#streamId);
  }
}

export class Router {
  readonly #db: LarkDb;
  readonly #edge: RouterEdge;
  readonly #brain: TurnBrain;
  readonly #dc: RouterDc | undefined;
  readonly #portraits: RouterOptions["portraits"];
  readonly #now: () => number;
  readonly #log: (line: string) => void;
  readonly #terminals = new Map<string, TerminalState>();
  readonly #models = new Map<string, string>();
  #streamSeq = 0;
  #closed = false;

  constructor(options: RouterOptions) {
    this.#db = options.db;
    this.#edge = options.edge;
    this.#brain = options.brain;
    this.#dc = options.dc;
    this.#portraits = options.portraits;
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
    const states = this.onlineTerminals(companionId);
    if (states.length === 0) return "offline";
    const state = states[0];
    if (state === undefined) return "offline";
    if (state.speaking) return "speaking";
    if (state.busy) return "thinking";
    if (state.listening) return "listening";
    return "idle";
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
          const story = parseTwee(row.twee);
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

  snapshot(companionId: string): RemoteSnapshot {
    const companion = this.#db.getCompanion(companionId) ?? this.defaultCompanion();
    if (companion === undefined) {
      return {
        companion: { id: companionId, name: "Lark", form: "", voice: "af_heart", listening: false, live_state: "offline" },
        terminals: [],
        stories: [],
        voices: VOICES.map((voice) => ({ ...voice })),
      };
    }
    let portrait: string | undefined;
    const portraitDir = this.#portraits?.dirFor(companion.portraitId);
    if (portraitDir !== undefined) {
      try {
        portrait = `data:image/png;base64,${readFileSync(join(portraitDir, "sheet.png")).toString("base64")}`;
      } catch {
        portrait = undefined;
      }
    }
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
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      if (state.drawer === "") await this.#renderHome(state, this.#statusFor(state));
    }
  }

  #statusFor(state: TerminalState): string {
    if (state.speaking) return "speaking";
    if (state.busy) return "thinking";
    if (state.listening) return "listening";
    return "idle";
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
      state.listening = false;
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
        await this.#renderHome(state, this.#statusFor(state));
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
      await this.#renderHome(state, this.#statusFor(state));
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
          await this.#renderDetail(state, "pair", "Pairing unavailable", error instanceof Error ? error.message : String(error));
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
      this.#log(`unpair: ${error instanceof Error ? error.message : String(error)}`);
    }
    state.drawer = "unpair";
    await this.#renderDrawer(state);
  }

  // ---- stories ------------------------------------------------------------

  async openStory(companionId: string, storyId: string, restart: boolean): Promise<void> {
    const row = this.#db.getStory(storyId);
    if (row === undefined) throw new Error(`story ${storyId} not found`);
    const story = parseTwee(row.twee);
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

  #prompt(text: string): string {
    return text.trim();
  }

  async #speculate(event: EdgeSpeculate): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined || state.busy || state.drawer === "story-reader" || event.transcript.trim() === "") return;
    state.speculation?.handle.abort();
    const sessionID = await this.#sessionFor(state.companionId);
    const capture = new DeltaCapture();
    const handle = this.#brain.startSpeculate({ sessionID, text: this.#prompt(event.transcript) }, (delta) => capture.push(delta));
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
        const iterator = this.#brain.runTurn({ sessionID, text: this.#prompt(transcript) });
        for (;;) {
          const step = await iterator.next();
          if (step.done) {
            full = step.value.text;
            break;
          }
          capture.push(step.value.text);
        }
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        this.#log(`turn: ${error instanceof Error ? error.message : String(error)}`);
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
    });
    if (spoken === "") {
      await this.#armListening(state);
      return;
    }

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
    });
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
      });
    } catch (error) {
      this.#log(`reply routing: ${error instanceof Error ? error.message : String(error)}`);
    }
  }

  async #playbackDone(event: EdgePlayback): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined) return;
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
        this.#log(`listen arm: ${error instanceof Error ? error.message : String(error)}`);
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
          armedUntil: 0,
        };
      }
      await this.speakPhrase(state.id, `Message from ${turn.sender}: ${turn.text}`);
    }
    try {
      const sessionID = await this.#sessionFor(turn.companionId);
      const iterator = this.#brain.runTurn({ sessionID, text: `${turn.sender} via Delta Chat: ${turn.text}` });
      let text = "";
      for (;;) {
        const step = await iterator.next();
        if (step.done) {
          text = step.value.text;
          break;
        }
      }
      return sanitizeSpeech(text);
    } catch (error) {
      this.#log(`dc turn: ${error instanceof Error ? error.message : String(error)}`);
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
        message.role === "assistant"
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
