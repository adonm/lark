// Request hooks and the speculation gate: state pruning, model failover, and
// the barrier that keeps mutating tools from running before a turn is promoted.
import { Plugin } from "@opencode/plugin";
import type { ModelRef } from "./opencode.ts";

export class SpeculationAbortedError extends Error {
  constructor(message = "speculative turn aborted") {
    super(message);
    this.name = "SpeculationAbortedError";
  }
}

export function sameModel(a: ModelRef | undefined, b: ModelRef | undefined): boolean {
  return a !== undefined && b !== undefined && a.providerID === b.providerID && a.id === b.id && a.variant === b.variant;
}

// ---- speculation gate ---------------------------------------------------

export class Speculation {
  readonly sessionID: string;
  readonly #promise: Promise<void>;
  #resolve!: () => void;
  #reject!: (error: Error) => void;
  #settled = false;
  #aborted = false;
  #abortReason: Error | undefined;

  constructor(sessionID: string) {
    this.sessionID = sessionID;
    this.#promise = new Promise<void>((resolvePromise, rejectPromise) => {
      this.#resolve = resolvePromise;
      this.#reject = rejectPromise;
    });
    // Aborting with no mutating tool in flight must not become an unhandled
    // rejection; real waiters still observe it.
    void this.#promise.catch(() => undefined);
  }

  get aborted(): boolean {
    return this.#aborted;
  }

  get abortReason(): Error | undefined {
    return this.#abortReason;
  }

  promote(): void {
    if (this.#settled) return;
    this.#settled = true;
    this.#resolve();
  }

  abort(reason: Error = new SpeculationAbortedError()): void {
    if (this.#settled) return;
    this.#settled = true;
    this.#aborted = true;
    this.#abortReason = reason;
    this.#reject(reason);
  }

  async wait(signal: AbortSignal | undefined): Promise<void> {
    if (this.#aborted) throw this.#abortReason ?? new SpeculationAbortedError();
    if (signal === undefined) {
      await this.#promise;
      return;
    }
    if (signal.aborted) throw new SpeculationAbortedError("turn cancelled");
    const cancelled = new Promise<never>((_, reject) => {
      signal.addEventListener(
        "abort",
        () => reject(new SpeculationAbortedError("turn cancelled")),
        { once: true },
      );
    });
    await Promise.race([this.#promise, cancelled]);
  }
}

export class SpeculationGate {
  readonly #active = new Map<string, Speculation>();

  begin(sessionID: string): Speculation {
    if (this.#active.has(sessionID)) {
      throw new Error(`a speculative turn is already active for ${sessionID}`);
    }
    const speculation = new Speculation(sessionID);
    this.#active.set(sessionID, speculation);
    return speculation;
  }

  promote(speculation: Speculation): void {
    this.#active.delete(speculation.sessionID);
    speculation.promote();
  }

  abort(speculation: Speculation, reason?: Error): void {
    this.#active.delete(speculation.sessionID);
    speculation.abort(reason);
  }

  close(): void {
    for (const speculation of this.#active.values()) speculation.abort(new SpeculationAbortedError("brain closed"));
    this.#active.clear();
  }

  /** Mutations wait for promotion; read-only tools never call this. */
  async mutate<T>(sessionID: string | undefined, signal: AbortSignal | undefined, run: () => T | Promise<T>): Promise<T> {
    const speculation = sessionID === undefined ? undefined : this.#active.get(sessionID);
    if (speculation === undefined) return run();
    await speculation.wait(signal);
    // Abort can win the race with a promotion; never execute a side effect after it.
    if (speculation.aborted) throw speculation.abortReason ?? new SpeculationAbortedError();
    return run();
  }
}

// ---- lark.context -------------------------------------------------------

interface RequestPart {
  type?: string;
  text?: string;
}
interface RequestMessage {
  role?: string;
  content?: string | readonly RequestPart[];
}

const STATE_PATTERN = /\n?\[state:[^\]]*\]/g;

function stripMessage(message: RequestMessage): void {
  if (message.role !== "user" || !Array.isArray(message.content)) return;
  for (const part of message.content as readonly RequestPart[]) {
    if (part.type === "text" && typeof part.text === "string" && part.text.includes("[state:")) {
      part.text = part.text.replace(STATE_PATTERN, "");
    }
  }
}

export function stripState(messages: readonly unknown[], keepLast: boolean): void {
  let lastUser = -1;
  if (keepLast) {
    for (let i = messages.length - 1; i >= 0; i--) {
      const message = messages[i] as RequestMessage | undefined;
      if (message?.role === "user") {
        lastUser = i;
        break;
      }
    }
  }
  for (let i = 0; i < messages.length; i++) {
    if (i === lastUser) continue;
    stripMessage(messages[i] as RequestMessage);
  }
}

export const contextPlugin: Plugin.Plugin = Plugin.define({
  id: "lark.context",
  async setup(ctx) {
    // A tool continuation resends the current user message, so keep its state.
    await ctx.session.hook("context", (event) => stripState(event.messages, true));
    // A compaction summary must not inherit one turn's clock, form or battery.
    await ctx.session.hook("compaction", (event) => stripState(event.messages, false));
  },
});

// ---- lark.model-fallback ------------------------------------------------

function rejectedStatus(status: number): boolean {
  return status === 429 || status === 503 || status === 403;
}

export function modelFallbackPlugin(agentID: string, primary: ModelRef, fallback: ModelRef): Plugin.Plugin {
  const responses = new Map<string, { status: number; model: ModelRef }>();
  const failedOver = new Set<string>();
  return Plugin.define({
    id: "lark.model-fallback",
    async setup(ctx) {
      await ctx.session.hook("prompt", (event) => {
        failedOver.delete(event.sessionID);
        responses.delete(event.sessionID);
      });
      await ctx.session.hook("http.response", (event) => {
        if (event.kind !== "primary" || event.agent !== agentID) return;
        if (rejectedStatus(event.response.status)) {
          responses.set(event.sessionID, { status: event.response.status, model: event.model });
        } else {
          responses.delete(event.sessionID);
        }
      });
      await ctx.session.hook("retry", (event) => {
        if (event.agent !== agentID) return;
        const recorded = responses.get(event.sessionID);
        if (recorded === undefined) return;
        if (!sameModel(recorded.model, event.model) || recorded.status !== event.error.status) return;
        responses.delete(event.sessionID);
        if (sameModel(event.model, fallback) || !sameModel(event.model, primary)) {
          event.decision = { retry: false };
          return;
        }
        // One retry onto the fallback model within the same turn.
        failedOver.add(event.sessionID);
        event.decision = { retry: true, delay: 0 };
      });
      await ctx.session.hook("experimental.model.resolve", (event) => {
        if (event.kind !== "primary" || event.agent !== agentID) return;
        if (!failedOver.has(event.sessionID)) return;
        if (sameModel(event.model, primary)) event.model = { ...fallback } as typeof event.model;
      });
    },
  });
}

