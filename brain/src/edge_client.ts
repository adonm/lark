// Unix-socket NDJSON client for the Go media edge. The edge owns hardware,
// DSP and playback timing; this client only forwards declarative screens,
// speech streams, recordings and RPCs, and reconnects transparently after an
// edge restart. A new connection receives `ready` plus a `terminal.connected`
// snapshot for every live board, so a brain restart never drops hardware.
import { createConnection, type Socket } from "node:net";
import { existsSync } from "node:fs";

export interface EdgeModelState {
  state: string;
  error?: string;
}

export interface EdgeReady {
  models: Record<string, EdgeModelState>;
}

export interface EdgeTerminal {
  terminal_id: string;
  board: string;
  firmware: string;
  width: number;
  height: number;
  shape: string;
  ip: string;
}

export interface EdgeTelemetry {
  terminal_id: string;
  wifi_rssi: number;
  battery_pct: number;
  charging: boolean;
  cpu_pct: number;
  free_heap: number;
  uptime_s: number;
}

export interface EdgeSpeculate {
  terminal_id: string;
  seq: number;
  transcript: string;
  speech_at_ms: number;
}

export interface EdgeCommit extends EdgeSpeculate {
  listen_window: boolean;
  audio_ms: number;
}

export interface EdgeCancel {
  terminal_id: string;
  seq: number;
  reason: string;
}

export interface EdgePlayback {
  terminal_id: string;
  stream_id: number;
  aborted: boolean;
  underruns_delta: number;
}

export interface EdgePointer {
  terminal_id: string;
  action: string;
  value: string;
}

export interface EdgeCaptureProgress {
  terminal_id: string;
  capture_id: string;
  remaining_s: number;
}

export interface EdgeCaptureDone {
  terminal_id: string;
  capture_id: string;
  path: string;
  seconds: number;
  error?: string;
}

export interface EdgeEventMap {
  ready: EdgeReady;
  "terminal.connected": EdgeTerminal;
  "terminal.disconnected": { terminal_id: string };
  "terminal.telemetry": EdgeTelemetry;
  "turn.speculate": EdgeSpeculate;
  "turn.commit": EdgeCommit;
  "turn.cancel": EdgeCancel;
  "turn.not_caught": { terminal_id: string };
  "pointer.action": EdgePointer;
  "playback.done": EdgePlayback;
  "capture.progress": EdgeCaptureProgress;
  "capture.done": EdgeCaptureDone;
  disconnected: { reason: string };
}

export interface EdgeHit {
  ID: string;
  Label?: string;
  X1: number;
  Y1: number;
  X2: number;
  Y2: number;
}

export interface EdgeHitsReply {
  rev: number;
  screen: string;
  mode: string;
  hits: EdgeHit[];
}

export interface EdgeHealthReply {
  models: Record<string, EdgeModelState>;
  terminals: number;
}

export interface ScreenItem {
  id: string;
  label: string;
  detail?: string;
  icon?: string;
  action?: string;
  value?: string;
}

export interface ScreenChoice {
  id: string;
  label: string;
  value: string;
}

export interface ScreenVoice {
  id: string;
  label: string;
}

export interface ScreenRow {
  title: string;
  text: string;
}

export interface Screen {
  kind: string;
  portrait_dir?: string;
  companion_name?: string;
  companion_form?: string;
  status?: string;
  caption?: string;
  listen_window?: boolean;
  page?: number;
  title?: string;
  heading?: string;
  body?: string;
  body_scale?: number;
  invite?: string;
  level?: number;
  current?: string;
  items?: ScreenItem[];
  choices?: ScreenChoice[];
  voices?: ScreenVoice[];
  rows?: ScreenRow[];
}

export interface RecStartOptions {
  listenWindow?: boolean;
  windowS?: number;
  followUpGraceMs?: number;
  companionName?: string;
}

type Listener<T> = (value: T) => void;

class TypedEmitter<Events extends object> {
  readonly #listeners = new Map<keyof Events, Set<Listener<unknown>>>();

  on<K extends keyof Events>(name: K, listener: Listener<Events[K]>): () => void {
    let set = this.#listeners.get(name);
    if (set === undefined) {
      set = new Set();
      this.#listeners.set(name, set);
    }
    set.add(listener as Listener<unknown>);
    return () => this.off(name, listener);
  }

  off<K extends keyof Events>(name: K, listener: Listener<Events[K]>): void {
    this.#listeners.get(name)?.delete(listener as Listener<unknown>);
  }

  emit<K extends keyof Events>(name: K, value: Events[K]): void {
    const set = this.#listeners.get(name);
    if (set === undefined) return;
    for (const listener of [...set]) {
      try {
        (listener as Listener<Events[K]>)(value);
      } catch {
        // A listener bug must not tear down the socket reader.
      }
    }
  }

  removeAll(): void {
    this.#listeners.clear();
  }
}

interface Pending {
  resolve: (value: Record<string, unknown>) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
}

const EVENT_TYPES = new Set<string>([
  "ready",
  "terminal.connected",
  "terminal.disconnected",
  "terminal.telemetry",
  "turn.speculate",
  "turn.commit",
  "turn.cancel",
  "turn.not_caught",
  "pointer.action",
  "playback.done",
  "capture.progress",
  "capture.done",
]);

export class EdgeRpcError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "EdgeRpcError";
  }
}

export interface EdgeClientOptions {
  socketPath: string;
  /** Reconnect delay bounds; tests shorten them. */
  minBackoffMs?: number;
  maxBackoffMs?: number;
  rpcTimeoutMs?: number;
  log?: (line: string) => void;
  /** Test hook; production connects to the Unix socket. */
  connect?: (path: string) => Socket;
}

export class EdgeClient extends TypedEmitter<EdgeEventMap> {
  readonly socketPath: string;
  readonly #minBackoff: number;
  readonly #maxBackoff: number;
  readonly #rpcTimeout: number;
  readonly #log: (line: string) => void;
  readonly #connect: (path: string) => Socket;
  readonly #pending = new Map<string, Pending>();

  #socket: Socket | undefined;
  #buffer = "";
  #seq = 0;
  #backoff: number;
  #timer: ReturnType<typeof setTimeout> | undefined;
  #stopped = false;
  #started = false;
  #connected = false;

  constructor(options: EdgeClientOptions) {
    super();
    this.socketPath = options.socketPath;
    this.#minBackoff = options.minBackoffMs ?? 200;
    this.#maxBackoff = options.maxBackoffMs ?? 5000;
    this.#rpcTimeout = options.rpcTimeoutMs ?? 5000;
    this.#backoff = this.#minBackoff;
    this.#log = options.log ?? (() => undefined);
    this.#connect = options.connect ?? ((path) => createConnection(path));
  }

  get connected(): boolean {
    return this.#connected;
  }

  start(): void {
    if (this.#started) return;
    this.#started = true;
    this.#stopped = false;
    this.#open();
  }

  async close(): Promise<void> {
    this.#stopped = true;
    this.#started = false;
    if (this.#timer !== undefined) {
      clearTimeout(this.#timer);
      this.#timer = undefined;
    }
    const socket = this.#socket;
    this.#socket = undefined;
    this.#connected = false;
    this.#failPending("edge client closed");
    if (socket !== undefined) {
      socket.removeAllListeners();
      socket.destroy();
    }
    this.removeAll();
  }

  #open(): void {
    if (this.#stopped) return;
    const socket = this.#connect(this.socketPath);
    this.#socket = socket;
    this.#buffer = "";
    socket.setEncoding("utf8");
    socket.setNoDelay(true);
    socket.on("connect", () => {
      this.#connected = true;
      this.#backoff = this.#minBackoff;
    });
    socket.on("data", (chunk: string) => this.#feed(chunk));
    socket.on("error", (error: Error) => {
      this.#log(`edge socket: ${error.message}`);
    });
    socket.on("close", () => {
      if (this.#socket !== socket) return;
      this.#connected = false;
      this.#socket = undefined;
      this.#failPending("edge disconnected");
      this.emit("disconnected", { reason: "edge socket closed" });
      this.#schedule();
    });
  }

  #schedule(): void {
    if (this.#stopped || this.#timer !== undefined) return;
    const delay = this.#backoff;
    this.#backoff = Math.min(this.#maxBackoff, Math.round(this.#backoff * 1.7));
    this.#timer = setTimeout(() => {
      this.#timer = undefined;
      this.#open();
    }, delay);
    this.#timer.unref();
  }

  #feed(chunk: string): void {
    this.#buffer += chunk;
    for (;;) {
      const index = this.#buffer.indexOf("\n");
      if (index < 0) return;
      const line = this.#buffer.slice(0, index).trim();
      this.#buffer = this.#buffer.slice(index + 1);
      if (line === "") continue;
      this.#handleLine(line);
    }
  }

  #handleLine(line: string): void {
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      this.#log("edge sent malformed JSON");
      return;
    }
    if (value === null || typeof value !== "object" || Array.isArray(value)) return;
    const message = value as Record<string, unknown>;
    const id = typeof message.id === "string" ? message.id : "";
    if (id !== "") {
      const pending = this.#pending.get(id);
      if (pending === undefined) return;
      this.#pending.delete(id);
      clearTimeout(pending.timer);
      if (message.ok === false) {
        pending.reject(new EdgeRpcError(typeof message.error === "string" ? message.error : "edge request failed"));
      } else {
        pending.resolve(message);
      }
      return;
    }
    const kind = typeof message.type === "string" ? message.type : "";
    if (!EVENT_TYPES.has(kind)) return;
    this.emit(kind as keyof EdgeEventMap, message as unknown as EdgeEventMap[keyof EdgeEventMap]);
  }

  #failPending(reason: string): void {
    for (const [id, pending] of this.#pending) {
      clearTimeout(pending.timer);
      pending.reject(new EdgeRpcError(reason));
      this.#pending.delete(id);
    }
  }

  #write(message: Record<string, unknown>): boolean {
    const socket = this.#socket;
    if (socket === undefined || !this.#connected) return false;
    try {
      socket.write(`${JSON.stringify(message)}\n`);
      return true;
    } catch {
      return false;
    }
  }

  #send(command: Record<string, unknown>): void {
    // Fire-and-forget streaming commands intentionally have no reply.
    this.#write({ ...command, id: "" });
  }

  #request(command: Record<string, unknown>, timeoutMs = this.#rpcTimeout): Promise<Record<string, unknown>> {
    const id = `b${++this.#seq}`;
    return new Promise<Record<string, unknown>>((resolve, reject) => {
      if (!this.#write({ ...command, id })) {
        reject(new EdgeRpcError("edge is not connected"));
        return;
      }
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new EdgeRpcError(`edge request ${String(command.cmd)} timed out`));
      }, timeoutMs);
      timer.unref();
      this.#pending.set(id, { resolve, reject, timer });
    });
  }

  // ---- terminal commands --------------------------------------------------

  async render(terminalId: string, screen: Screen): Promise<void> {
    await this.#request({ cmd: "terminal.render", terminal_id: terminalId, screen });
  }

  async speakStart(terminalId: string, streamId: number, voice: string): Promise<void> {
    await this.#request({ cmd: "terminal.speak_start", terminal_id: terminalId, stream_id: streamId, voice });
  }

  speakChunk(terminalId: string, streamId: number, text: string): void {
    this.#send({ cmd: "terminal.speak_chunk", terminal_id: terminalId, stream_id: streamId, text });
  }

  speakEnd(terminalId: string, streamId: number): void {
    this.#send({ cmd: "terminal.speak_end", terminal_id: terminalId, stream_id: streamId });
  }

  async stopAudio(terminalId: string): Promise<void> {
    await this.#request({ cmd: "terminal.stop_audio", terminal_id: terminalId });
  }

  filler(terminalId: string, voice: string): void {
    this.#send({ cmd: "terminal.filler", terminal_id: terminalId, voice });
  }

  previewVoice(terminalId: string, voice: string): void {
    this.#send({ cmd: "terminal.preview_voice", terminal_id: terminalId, voice });
  }

  chime(terminalId: string, kind: string): void {
    this.#send({ cmd: "terminal.chime", terminal_id: terminalId, kind });
  }

  power(terminalId: string, brightness: number): void {
    this.#send({ cmd: "terminal.power", terminal_id: terminalId, brightness });
  }

  volume(terminalId: string, level: number): void {
    this.#send({ cmd: "terminal.volume", terminal_id: terminalId, level });
  }

  async recStart(terminalId: string, options: RecStartOptions = {}): Promise<void> {
    await this.#request({
      cmd: "terminal.rec_start",
      terminal_id: terminalId,
      listen_window: options.listenWindow ?? false,
      window_s: options.windowS ?? 0,
      follow_up_grace_ms: options.followUpGraceMs ?? 0,
      companion_name: options.companionName ?? "",
    });
  }

  async recCancel(terminalId: string): Promise<void> {
    await this.#request({ cmd: "terminal.rec_cancel", terminal_id: terminalId });
  }

  async hits(terminalId: string): Promise<EdgeHitsReply> {
    const reply = await this.#request({ cmd: "terminal.hits", terminal_id: terminalId }, 3000);
    return {
      rev: typeof reply.rev === "number" ? reply.rev : 0,
      screen: typeof reply.screen === "string" ? reply.screen : "",
      mode: typeof reply.mode === "string" ? reply.mode : "",
      hits: Array.isArray(reply.hits) ? (reply.hits as EdgeHit[]) : [],
    };
  }

  // ---- shared RPCs --------------------------------------------------------

  async transcribe(path: string): Promise<string> {
    const reply = await this.#request({ cmd: "stt.transcribe", path }, 30_000);
    if (typeof reply.text !== "string") throw new EdgeRpcError("stt.transcribe returned no text");
    return reply.text;
  }

  async ttsWav(voice: string, text: string, outPath: string): Promise<string> {
    const reply = await this.#request({ cmd: "tts.wav", voice, text, out_path: outPath }, 60_000);
    if (typeof reply.path !== "string") throw new EdgeRpcError("tts.wav returned no path");
    return reply.path;
  }

  async normalizePortrait(dir: string): Promise<number> {
    const reply = await this.#request({ cmd: "portrait.normalize", dir }, 60_000);
    return typeof reply.colors === "number" ? reply.colors : 0;
  }

  async captureStart(terminalId: string, outPath: string, seconds: number): Promise<void> {
    await this.#request({ cmd: "capture.start", terminal_id: terminalId, out_path: outPath, seconds });
  }

  async captureStop(terminalId: string): Promise<void> {
    await this.#request({ cmd: "capture.stop", terminal_id: terminalId });
  }

  async health(): Promise<EdgeHealthReply> {
    const reply = await this.#request({ cmd: "health" });
    const models =
      reply.models !== null && typeof reply.models === "object" && !Array.isArray(reply.models)
        ? (reply.models as Record<string, EdgeModelState>)
        : {};
    return { models, terminals: typeof reply.terminals === "number" ? reply.terminals : 0 };
  }

  /** Wait for the initial ready event, bounded by `timeoutMs`. */
  async waitReady(timeoutMs = 5000): Promise<EdgeReady> {
    return await new Promise<EdgeReady>((resolve, reject) => {
      const timer = setTimeout(() => {
        off();
        reject(new Error("edge ready timed out"));
      }, timeoutMs);
      timer.unref();
      const off = this.on("ready", (ready) => {
        clearTimeout(timer);
        off();
        resolve(ready);
      });
      if (!this.#started) this.start();
    });
  }

  static socketExists(path: string): boolean {
    return existsSync(path);
  }
}
