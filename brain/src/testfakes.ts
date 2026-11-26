// Deterministic in-process doubles for router, HTTP and portrait tests. They
// implement only the interfaces the production classes consume; the binary
// protocol itself is exercised by e2e.test.ts against the real Go edge.
import type {
  EdgeEventMap,
  EdgeHealthReply,
  EdgeHit,
  Screen,
} from "./edge_client.ts";
import type { EdgeCaptureDone, EdgeCaptureProgress } from "./edge_client.ts";
import type { RouterEdge, TurnBrain, TurnDelta, TurnHandle, TurnResult } from "./router.ts";

type Listener = (event: unknown) => void;

export class FakeEdge implements RouterEdge {
  readonly #listeners = new Map<string, Set<Listener>>();
  readonly screens: Array<{ terminalId: string; screen: Screen }> = [];
  readonly spoken: Array<{ terminalId: string; streamId: number; text: string }> = [];
  readonly starts: Array<{ terminalId: string; streamId: number; voice: string }> = [];
  readonly ends: Array<{ terminalId: string; streamId: number }> = [];
  readonly recStarts: Array<{ terminalId: string; options: unknown }> = [];
  readonly volumes: Array<{ terminalId: string; level: number }> = [];
  readonly previews: Array<{ terminalId: string; voice: string }> = [];
  readonly powers: Array<{ terminalId: string; brightness: number }> = [];
  readonly hitsReply: EdgeHit[] = [];
  readonly metronomes: Array<{ terminalId: string; bpm: number; beats: number; durationS: number }> = [];
  readonly tunes: Array<{ terminalId: string; tempo: number; style: string; notes: unknown[] }> = [];
  readonly captures: Array<{ terminalId: string; path: string; seconds: number }> = [];
  captureStops = 0;
  transcribeText = "";
  normalizeCalls: string[] = [];

  on<K extends keyof EdgeEventMap>(name: K, listener: (event: EdgeEventMap[K]) => void): () => void {
    let set = this.#listeners.get(name);
    if (set === undefined) {
      set = new Set();
      this.#listeners.set(name, set);
    }
    set.add(listener as Listener);
    return () => set?.delete(listener as Listener);
  }

  emit<K extends keyof EdgeEventMap>(name: K, event: EdgeEventMap[K]): void {
    for (const listener of this.#listeners.get(name) ?? []) listener(event);
  }

  async render(terminalId: string, screen: Screen): Promise<void> {
    this.screens.push({ terminalId, screen });
  }

  async speakStart(terminalId: string, streamId: number, voice: string): Promise<void> {
    this.starts.push({ terminalId, streamId, voice });
  }

  speakChunk(terminalId: string, streamId: number, text: string): void {
    this.spoken.push({ terminalId, streamId, text });
  }

  speakEnd(terminalId: string, streamId: number): void {
    this.ends.push({ terminalId, streamId });
  }

  async stopAudio(_terminalId: string): Promise<void> {}

  chime(_terminalId: string, _kind: string): void {}

  power(terminalId: string, brightness: number): void {
    this.powers.push({ terminalId, brightness });
  }

  volume(terminalId: string, level: number): void {
    this.volumes.push({ terminalId, level });
  }

  async recStart(terminalId: string, options: unknown = {}): Promise<void> {
    this.recStarts.push({ terminalId, options });
  }

  async recCancel(_terminalId: string): Promise<void> {}

  previewVoice(terminalId: string, voice: string): void {
    this.previews.push({ terminalId, voice });
  }

  async captureStart(terminalId: string, path: string, seconds: number): Promise<void> {
    this.captures.push({ terminalId, path, seconds });
  }

  async captureStop(_terminalId: string): Promise<void> {
    this.captureStops++;
  }

  async metronome(terminalId: string, bpm: number, beats: number, durationS: number): Promise<number> {
    this.metronomes.push({ terminalId, bpm, beats, durationS });
    return this.metronomes.length;
  }

  async playTune(terminalId: string, tempo: number, style: string, notes: unknown[]): Promise<number> {
    this.tunes.push({ terminalId, tempo, style, notes });
    return this.tunes.length;
  }

  async normalizePortrait(dir: string): Promise<number> {
    this.normalizeCalls.push(dir);
    return 12;
  }

  async hits(_terminalId: string): Promise<{ rev: number; screen: string; mode: string; hits: EdgeHit[] }> {
    return { rev: 1, screen: "home", mode: "idle", hits: this.hitsReply };
  }

  async transcribe(_path: string): Promise<string> {
    return this.transcribeText;
  }

  async health(): Promise<EdgeHealthReply> {
    return { models: {}, terminals: 0 };
  }

  emitCaptureDone(event: EdgeCaptureDone): void {
    this.emit("capture.done", event);
  }

  emitCaptureProgress(event: EdgeCaptureProgress): void {
    this.emit("capture.progress", event);
  }

  lastScreen(): Screen | undefined {
    return this.screens.at(-1)?.screen;
  }
}

export interface FakeTurn {
  deltas: string[];
  text: string;
  delay?: number;
}

export class FakeBrain implements TurnBrain {
  readonly sessions: Array<{ id: string; name: string; personality: string }> = [];
  readonly inputs: Array<{ sessionID: string; text: string }> = [];
  readonly turns: FakeTurn[] = [];
  speculated: Array<TurnHandle & { transcript: string }> = [];
  readonly promoted: string[] = [];
  readonly aborted: string[] = [];
  readonly missingSessions = new Set<string>();

  nextTurn(text: string, deltas?: string[]): void {
    this.turns.push({ deltas: deltas ?? [text], text });
  }

  async createSession(companion: { id: string; name: string; personality: string }): Promise<string> {
    this.sessions.push(companion);
    return `sess-${companion.id}`;
  }

  async sessionExists(sessionID: string): Promise<boolean> {
    return !this.missingSessions.has(sessionID);
  }

  async *runTurn(input: { sessionID: string; text: string }): AsyncGenerator<TurnDelta, TurnResult> {
    this.inputs.push(input);
    const turn = this.turns.shift() ?? { deltas: ["Okay."], text: "Okay." };
    for (const delta of turn.deltas) yield { type: "delta", text: delta };
    return { sessionID: input.sessionID, text: turn.text, model: { providerID: "fake", id: "fake" }, llmFirstMs: 0, llmTotalMs: 0 };
  }

  startSpeculate(
    input: { sessionID: string; text: string },
    onDelta?: (delta: string) => void,
  ): TurnHandle {
    const turn = this.turns.shift() ?? { deltas: ["Okay."], text: "Okay." };
    this.inputs.push(input);
    const result = Promise.withResolvers<TurnResult>();
    let aborted = false;
    queueMicrotask(() => {
      for (const delta of turn.deltas) onDelta?.(delta);
      if (aborted) return;
      result.resolve({ sessionID: input.sessionID, text: turn.text, model: { providerID: "fake", id: "fake" }, llmFirstMs: 0, llmTotalMs: 0 });
    });
    const handle: TurnHandle & { transcript: string } = {
      sessionID: input.sessionID,
      transcript: input.text,
      result: result.promise,
      promote: () => {
        this.promoted.push(input.text);
      },
      abort: (reason?: Error) => {
        aborted = true;
        this.aborted.push(input.text);
        result.reject(reason ?? new Error("aborted"));
      },
    };
    this.speculated.push(handle);
    return handle;
  }
}

/** Wait for queued promise continuations inside the router. */
export async function settle(rounds = 8): Promise<void> {
  for (let i = 0; i < rounds; i++) await new Promise((resolve) => setImmediate(resolve));
}
