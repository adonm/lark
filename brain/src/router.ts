// Voice turns, device menus, listening policy, story reader and companion
// state. The router is the only place that knows how edge events map to a
// companion; HTTP, Delta Chat and the CLI all call the same methods here.
//
// Lifecycle, state and the public facade live in this file. Voice/turn policy
// is in router_voice.ts, and every screen/menu/story action is in router_ui.ts;
// both modules receive a small context object from the constructor.
import { existsSync } from "node:fs";
import { join } from "node:path";
import type {
  EdgeEventMap,
  EdgePointer,
  EdgeTelemetry,
  EdgeTerminal,
  Screen,
  TuneNote,
} from "./edge_client.ts";
import type { SongMatch } from "./song_id.ts";
import type { Companion, LarkDb, Message, StoryRow } from "./db.ts";
import { parseTwee, type Story } from "./stories.ts";
import type { DcPeerTurn } from "./deltachat.ts";
import type { SpeculationHandle, TurnDelta, TurnResult } from "./opencode.ts";
import type { Timer } from "./timers.ts";
import type { PortraitManager } from "./portraits.ts";
import { nameMatches } from "./matching.ts";
import { sanitizeSpeech } from "./speech.ts";
import { NOT_CAUGHT_LINE, statusOf, type TerminalState } from "./router_internal.ts";
import { errMessage } from "./util.ts";
import * as ui from "./router_ui.ts";
import * as voice from "./router_voice.ts";

export { nameMatches, sanitizeSpeech };
export { VOICES } from "./router_ui.ts";
export { FOLLOW_UP_GRACE_MS, LISTEN_WINDOW_MS, REPLY_CONTEXT_MS } from "./router_voice.ts";

export type { TurnDelta, TurnResult };
export type TurnHandle = SpeculationHandle;

export { NOT_CAUGHT_LINE };
const DEFAULT_BRIGHTNESS = 85;
const PORTRAIT_PATH = "/remote/api/portrait";

export interface TurnBrain {
  createSession(companion: { id: string; name: string; personality: string }): Promise<string>;
  /** False when a stored session disappeared with an old OpenCode store. */
  sessionExists(sessionID: string): Promise<boolean>;
  /** Re-attach the companion instruction to a persisted session. */
  setCompanion(sessionID: string, companion: { id: string; name: string; personality: string }): Promise<void>;
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
    /** Compact token-scoped URL; the PNG itself is no longer in the SSE body. */
    portraitUrl?: string;
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
  // Portrait presence and parsed stories change only when their source changes,
  // so remote snapshots must not re-read or re-parse them on every event.
  readonly #portraitCache = new Map<string, boolean>();
  readonly #storyCache = new Map<string, { updatedAt: number; story: Story }>();
  readonly #subscribers = new Set<() => void>();
  readonly #ui: ui.UiContext;
  readonly #voice: voice.VoiceContext;
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
    this.#ui = {
      db: this.#db,
      edge: this.#edge,
      dc: this.#dc,
      portraits: this.#portraits,
      terminals: () => this.#terminals.values(),
      now: this.#now,
      log: this.#log,
      notify: () => this.#notifyStateChange(),
      clearPortraitCache: () => this.#portraitCache.clear(),
      storySummaries: (companionId) => this.storySummaries(companionId),
      parseStory: (row) => this.#parseStory(row),
      armListening: (state, startRec) => voice.armListening(this.#voice, state, startRec),
    };
    this.#voice = {
      db: this.#db,
      edge: this.#edge,
      brain: this.#brain,
      dc: this.#dc,
      identifySong: this.#identify,
      terminals: () => this.#terminals.values(),
      terminal: (terminalId) => this.#terminals.get(terminalId),
      onlineTerminals: (companionId) => this.onlineTerminals(companionId),
      now: this.#now,
      log: this.#log,
      notify: () => this.#notifyStateChange(),
      closed: () => this.#closed,
      renderHome: (state, status, caption) => ui.renderHome(this.#ui, state, status, caption),
      spokenStoryChoice: (state, transcript) => ui.spokenStoryChoice(this.#ui, state, transcript),
      nextStream: () => ++this.#streamSeq,
    };
    this.#bind();
  }

  #bind(): void {
    this.#edge.on("ready", (event) => {
      for (const [name, state] of Object.entries(event.models)) this.#models.set(name, state.state);
    });
    this.#edge.on("terminal.connected", (event) => {
      this.#log(
        `terminal.connected id=${event.terminal_id} board=${event.board} fw=${event.firmware} ip=${event.ip} reconnects=${event.reconnects ?? 0}`,
      );
      void this.#connected(event);
    });
    this.#edge.on("terminal.disconnected", (event) => {
      this.#log(`terminal.disconnected id=${event.terminal_id}`);
      void this.#disconnected(event.terminal_id);
    });
    this.#edge.on("terminal.telemetry", (event) => this.#telemetry(event));
    this.#edge.on("pointer.action", (event) => {
      void this.#pointer(event).catch((error: unknown) => this.#log(`pointer: ${String(error)}`));
    });
    this.#edge.on("turn.speculate", (event) => {
      void voice.speculate(this.#voice, event).catch((error: unknown) => this.#log(`speculate: ${String(error)}`));
    });
    this.#edge.on("turn.commit", (event) => {
      void voice.commit(this.#voice, event).catch((error: unknown) => this.#log(`commit: ${String(error)}`));
    });
    this.#edge.on("turn.cancel", (event) => void voice.cancel(this.#voice, event.terminal_id, event.reason));
    this.#edge.on("turn.not_caught", (event) => void this.speakPhrase(event.terminal_id, NOT_CAUGHT_LINE));
    this.#edge.on("playback.done", (event) => void voice.playbackDone(this.#voice, event));
  }

  get models(): Map<string, string> {
    return new Map(this.#models);
  }

  /** Listen for state mutations; HTTP coalesces bursts into one SSE event. */
  subscribe(listener: () => void): () => void {
    this.#subscribers.add(listener);
    return () => {
      this.#subscribers.delete(listener);
    };
  }

  #notifyStateChange(): void {
    for (const listener of this.#subscribers) {
      try {
        listener();
      } catch {
        // A broken subscriber must not break the mutation that notified it.
      }
    }
  }

  async close(): Promise<void> {
    this.#closed = true;
    this.#subscribers.clear();
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
    const rows = this.#db.listTerminalsWithVolume(companionId);
    const volume = new Map(rows.map((row) => [row.id, row.volume]));
    const missing = [...this.#terminals.values()]
      .filter((state) => state.companionId === companionId && !volume.has(state.id))
      .map((state) => state.id);
    for (const [id, level] of this.#db.getTerminalVolumes(missing)) volume.set(id, level);
    for (const state of this.#terminals.values()) {
      if (state.companionId !== companionId) continue;
      seen.add(state.id);
      out.push({
        id: state.id,
        name: state.id,
        online: true,
        volume: volume.get(state.id) ?? 55,
        ...(state.telemetry === undefined
          ? {}
          : { battery_pct: state.telemetry.battery_pct, charging: state.telemetry.charging }),
      });
    }
    for (const row of rows) {
      if (seen.has(row.id)) continue;
      out.push({ id: row.id, name: row.id, online: false, volume: row.volume });
    }
    out.sort((a, b) => a.id.localeCompare(b.id));
    return out;
  }

  storySummaries(companionId: string): StorySummary[] {
    return this.#db.listStoriesWithProgress(companionId).map(({ story: row, progress }) => {
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

  /** The portrait id is a content key; only its presence needs caching. */
  #portraitUrl(companion: Companion | undefined): string | undefined {
    if (companion === undefined || companion.portraitId === "") return undefined;
    const id = companion.portraitId;
    let present = this.#portraitCache.get(id);
    if (present === undefined) {
      const dir = this.#portraits?.dirFor(id);
      present = dir !== undefined && existsSync(join(dir, "sheet.png"));
      this.#portraitCache.set(id, present);
    }
    if (!present) return undefined;
    return `${PORTRAIT_PATH}?v=${encodeURIComponent(id)}`;
  }

  snapshot(companionId: string): RemoteSnapshot {
    const companion = this.#db.getCompanion(companionId) ?? this.defaultCompanion();
    if (companion === undefined) {
      return {
        companion: { id: companionId, name: "Lark", form: "", voice: "af_heart", listening: false, live_state: "offline" },
        terminals: [],
        stories: [],
        voices: ui.VOICES.map((voice) => ({ ...voice })),
      };
    }
    const portraitUrl = this.#portraitUrl(companion);
    return {
      companion: {
        id: companion.id,
        name: companion.name,
        form: companion.form,
        voice: companion.voice === "" ? "af_heart" : companion.voice,
        listening: companion.listening,
        live_state: this.liveState(companion.id),
        ...(portraitUrl === undefined ? {} : { portraitUrl }),
      },
      terminals: this.remoteTerminals(companion.id),
      stories: this.storySummaries(companion.id).map((story) => ({
        name: story.id,
        title: story.title,
        passages: story.passages,
        progress: story.progress,
        ...(story.passage === undefined ? {} : { passage: story.passage }),
      })),
      voices: ui.VOICES.map((voice) => ({ ...voice })),
    };
  }

  diagnostics(): Record<string, unknown> {
    return {
      companions: this.#db.listCompanions().length,
      terminals: [...this.#terminals.values()].map((state) => ({
        id: state.id,
        companion_id: state.companionId,
        reconnects: state.info.reconnects ?? 0,
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
      previousBrightness: DEFAULT_BRIGHTNESS,
      specSeq: 0,
    };
    this.#terminals.set(info.terminal_id, state);
    this.#edge.power(info.terminal_id, DEFAULT_BRIGHTNESS);
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
    if (companion !== undefined && companion.listening) await voice.armListening(this.#voice, state);
    else await ui.renderHome(this.#ui, state, "idle");
    this.#notifyStateChange();
  }

  async #disconnected(terminalId: string): Promise<void> {
    const state = this.#terminals.get(terminalId);
    if (state !== undefined) {
      if (state.listenTimer !== undefined) clearTimeout(state.listenTimer);
      state.specSeq++;
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
    this.#notifyStateChange();
  }

  #telemetry(event: EdgeTelemetry): void {
    const state = this.#terminals.get(event.terminal_id);
    if (state !== undefined) {
      state.telemetry = event;
      this.#notifyStateChange();
    }
  }

  async #pointer(event: EdgePointer): Promise<void> {
    const state = this.#terminals.get(event.terminal_id);
    if (state === undefined) return;
    await ui.handlePointer(this.#ui, state, event.action, event.value);
  }

  async refreshCompanion(companionId: string): Promise<void> {
    const companion = this.#db.getCompanion(companionId);
    if (companion !== undefined) {
      for (const session of this.#db.listSessions()) {
        if (session.companionId !== companion.id.toLowerCase()) continue;
        try {
          await this.#brain.setCompanion(session.sessionId, {
            id: companion.id,
            name: companion.name,
            personality: companion.personality,
          });
        } catch (error) {
          // A stale session from an old store must not block the screen refresh.
          this.#log(`companion.refresh failed session=${session.sessionId}: ${errMessage(error)}`);
        }
      }
    }
    await ui.refreshCompanion(this.#ui, companionId);
  }

  // ---- stories ------------------------------------------------------------

  async openStory(companionId: string, storyId: string, restart: boolean): Promise<void> {
    await ui.openStory(this.#ui, companionId, storyId, restart);
  }

  async restartStory(companionId: string, storyId: string): Promise<void> {
    await this.openStory(companionId, storyId, true);
  }

  // ---- music and song identification --------------------------------------

  async audioMetronome(companionId: string, bpm: number, beats: number, durationS: number): Promise<Record<string, unknown>> {
    return await voice.audioMetronome(this.#voice, companionId, bpm, beats, durationS);
  }

  async audioTune(
    companionId: string,
    tempo: number,
    style: string,
    notes: TuneNote[],
    title?: string,
  ): Promise<Record<string, unknown>> {
    return await voice.audioTune(this.#voice, companionId, tempo, style, notes, title);
  }

  async audioIdentify(companionId: string, seconds: number): Promise<SongMatch & Record<string, unknown>> {
    return await voice.audioIdentify(this.#voice, companionId, seconds);
  }

  async audioStop(companionId: string): Promise<Record<string, unknown>> {
    return await voice.audioStop(this.#voice, companionId);
  }

  // ---- shared commands ----------------------------------------------------

  async sayNow(companionId: string, text: string): Promise<void> {
    await voice.sayNow(this.#voice, companionId, text);
  }

  async speakPhrase(terminalId: string, text: string): Promise<void> {
    await voice.speakPhrase(this.#voice, terminalId, text);
  }

  async wakeTimer(timer: Timer): Promise<void> {
    await voice.wakeTimer(this.#voice, timer);
  }

  async setListening(companionId: string, listening: boolean): Promise<void> {
    await voice.setListening(this.#voice, companionId, listening);
  }

  setVolume(terminalId: string, level: number): void {
    const clamped = Math.max(0, Math.min(100, Math.round(level)));
    this.#db.setTerminalVolume(terminalId, clamped);
    this.#edge.volume(terminalId, clamped);
    this.#notifyStateChange();
  }

  async setVoice(companionId: string, voiceId: string, preview: boolean): Promise<void> {
    await ui.setVoice(this.#ui, companionId, voiceId, preview);
  }

  async notifyDcTurn(turn: DcPeerTurn & { accountId?: number; chatId?: number }): Promise<string | undefined> {
    return await voice.notifyDcTurn(this.#voice, turn);
  }

  history(companionId: string, channel: "all" | "in_person" | "deltachat", contact: string, q: string, limit: number, cursor?: { beforeTs: number; beforeId: string }): { items: Array<Record<string, unknown>>; has_more: boolean; next_cursor?: string } {
    const page = this.#db.listRemoteMessages({
      companionId,
      channel,
      contact,
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
