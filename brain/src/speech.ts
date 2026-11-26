// Streaming TTS chunking and speculative-delta capture.
export interface SpeechEdge {
  speakStart(terminalId: string, streamId: number, voice: string): Promise<void>;
  speakChunk(terminalId: string, streamId: number, text: string): void;
  speakEnd(terminalId: string, streamId: number): void;
}

export function sanitizeSpeech(text: string): string {
  return text
    .replace(/```[\s\S]*?```/g, " ")
    .replace(/[*_#`>]+/g, "")
    .replace(/\[silent\]/gi, "")
    .replace(/\s+/g, " ")
    .trim();
}

export interface Sink {
  push(text: string): void;
}

/** Accumulates deltas until a turn is promoted, then forwards them in order. */
export class DeltaCapture {
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

export class SpeechStream implements Sink {
  readonly #edge: SpeechEdge;
  readonly #terminalId: string;
  readonly #voice: string;
  readonly #streamId: number;
  #chain: Promise<void> = Promise.resolve();
  #started = false;
  #buffer = "";
  #closed = false;

  constructor(edge: SpeechEdge, terminalId: string, voice: string, streamId: number) {
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
