// Voice turns: sessions, speculation, the listening window, follow-up name
// matching, speech playback, music and song identification. Everything here
// mutates the terminal state passed in; the router owns lifecycle and screens.
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type {
  EdgeCaptureDone,
  EdgeCommit,
  EdgePlayback,
  EdgeSpeculate,
  TuneNote,
} from "./edge_client.ts";
import type { LarkDb } from "./db.ts";
import type { DcPeerTurn } from "./deltachat.ts";
import type { RouterDc, RouterEdge, TurnBrain } from "./router.ts";
import type { SongMatch } from "./song_id.ts";
import type { TerminalState } from "./router_internal.ts";
import type { TurnResult } from "./opencode.ts";
import { nameMatches } from "./matching.ts";
import { DeltaCapture, SpeechStream, sanitizeSpeech } from "./speech.ts";
import { drain, errMessage } from "./util.ts";

export const LISTEN_WINDOW_MS = 30_000;
export const FOLLOW_UP_GRACE_MS = 10_000;
export const REPLY_CONTEXT_MS = 60_000;

export interface VoiceContext {
  db: LarkDb;
  edge: RouterEdge;
  brain: TurnBrain;
  dc: RouterDc | undefined;
  identifySong: ((wavPath: string) => Promise<SongMatch>) | undefined;
  terminals: () => Iterable<TerminalState>;
  terminal: (terminalId: string) => TerminalState | undefined;
  onlineTerminals: (companionId?: string) => TerminalState[];
  now: () => number;
  log: (line: string) => void;
  notify: () => void;
  closed: () => boolean;
  renderHome: (state: TerminalState, status: string, caption?: string) => Promise<void>;
  spokenStoryChoice: (state: TerminalState, transcript: string) => Promise<boolean>;
  nextStream: () => number;
}

function isSessionNotFound(error: unknown): boolean {
  return /session not found/i.test(errMessage(error));
}

async function sessionFor(ctx: VoiceContext, companionId: string): Promise<string> {
  // The sessions table is authoritative. A stale id from an old OpenCode
  // store is only discovered when prompt fails, then runTurn rebinds.
  const stored = ctx.db.getSession(companionId);
  if (stored !== undefined) return stored.sessionId;
  const companion = ctx.db.getCompanion(companionId);
  if (companion === undefined) return Promise.reject(new Error(`companion ${companionId} not found`));
  return ctx.brain.createSession({ id: companion.id, name: companion.name, personality: companion.personality });
}

export async function runTurn(
  ctx: VoiceContext,
  companionId: string,
  text: string,
  onDelta?: (delta: string) => void,
): Promise<TurnResult> {
  const sessionID = await sessionFor(ctx, companionId);
  try {
    return await drain(ctx.brain.runTurn({ sessionID, text }), (delta) => onDelta?.(delta.text));
  } catch (error) {
    if (!isSessionNotFound(error)) throw error;
    // The persisted session predates this OpenCode store. Drop it and retry
    // once with a fresh session; every later turn trusts the table again.
    ctx.db.deleteSession(companionId);
    const fresh = await sessionFor(ctx, companionId);
    return await drain(ctx.brain.runTurn({ sessionID: fresh, text }), (delta) => onDelta?.(delta.text));
  }
}

export async function speculate(ctx: VoiceContext, event: EdgeSpeculate): Promise<void> {
  const state = ctx.terminal(event.terminal_id);
  if (state === undefined || state.busy || state.drawer === "story-reader" || event.transcript.trim() === "") return;
  state.speculation?.handle.abort();
  const sessionID = await sessionFor(ctx, state.companionId);
  const capture = new DeltaCapture();
  const handle = ctx.brain.startSpeculate({ sessionID, text: event.transcript.trim() }, (delta) => capture.push(delta));
  state.speculation = { transcript: event.transcript.trim(), stream: capture, handle };
  void handle.result.catch(() => undefined);
}

export async function commit(ctx: VoiceContext, event: EdgeCommit): Promise<void> {
  const state = ctx.terminal(event.terminal_id);
  if (state === undefined || ctx.closed() || state.busy) return;
  const companion = ctx.db.getCompanion(state.companionId);
  if (companion === undefined) return;
  state.listening = false;
  ctx.notify();

  // A silent story reader uses spoken choice matching and never synthesizes
  // the passage itself.
  if (state.drawer === "story-reader") {
    const advanced = await ctx.spokenStoryChoice(state, event.transcript);
    if (advanced) return;
    if (/\b(stop|menu|home)\b/i.test(event.transcript)) {
      state.story = undefined;
      state.drawer = "";
      await ctx.renderHome(state, "idle");
    }
    return;
  }

  if (event.listen_window && companion.listening) {
    const elapsed = ctx.now() - state.listeningSince;
    if (elapsed > FOLLOW_UP_GRACE_MS && !nameMatches(event.transcript, companion.name)) {
      await armListening(ctx, state);
      return;
    }
  }

  const transcript = event.transcript.trim();
  if (transcript === "" || /^\[silent\]$/i.test(transcript)) {
    await armListening(ctx, state);
    return;
  }

  state.busy = true;
  ctx.notify();
  await ctx.renderHome(state, "thinking");
  ctx.edge.chime(state.id, "think");

  const speculation = state.speculation;
  state.speculation = undefined;
  const matching = speculation !== undefined && speculation.transcript === transcript;
  if (speculation !== undefined && !matching) speculation.handle.abort();

  const controller = new AbortController();
  state.turn = controller;
  const speech = new SpeechStream(
    ctx.edge,
    state.id,
    companion.voice === "" ? "af_heart" : companion.voice,
    ctx.nextStream(),
  );
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
      const result = await runTurn(ctx, state.companionId, transcript, (delta) => capture.push(delta));
      full = result.text;
    }
  } catch (error) {
    if (matching && isSessionNotFound(error)) {
      // The speculative turn found a stale binding; rebind and run once.
      ctx.db.deleteSession(state.companionId);
      try {
        const result = await runTurn(ctx, state.companionId, transcript, (delta) => capture.push(delta));
        full = result.text;
      } catch (retryError) {
        if (!controller.signal.aborted) ctx.log(`turn: ${errMessage(retryError)}`);
      }
    } else if (!controller.signal.aborted) {
      ctx.log(`turn: ${errMessage(error)}`);
    }
  } finally {
    state.turn = undefined;
    state.busy = false;
    ctx.notify();
  }
  await speech.end();

  const spoken = sanitizeSpeech(full);
  ctx.db.appendMessage({
    id: `turn:${state.id}:${ctx.now()}:user`,
    companionId: state.companionId,
    role: "user",
    sender: "Someone",
    text: transcript,
    source: "voice",
    channel: "in_person",
    cardTitle: "",
    tsMs: ctx.now(),
    peer: "",
  });
  if (spoken === "") {
    await armListening(ctx, state);
    return;
  }

  ctx.db.appendMessage({
    id: `turn:${state.id}:${ctx.now()}:reply`,
    companionId: state.companionId,
    role: "assistant",
    sender: companion.name,
    text: full,
    source: "voice",
    channel: "in_person",
    cardTitle: "",
    tsMs: ctx.now(),
    peer: "",
  });
  state.caption = spoken.slice(0, 120);
  state.speaking = true;
  ctx.notify();
  await ctx.renderHome(state, "speaking", state.caption);

  if (state.reply !== undefined && ctx.now() <= state.reply.armedUntil) {
    await routeReply(ctx, state, full);
  }
}

async function routeReply(ctx: VoiceContext, state: TerminalState, text: string): Promise<void> {
  const reply = state.reply;
  if (reply === undefined || ctx.dc === undefined) return;
  try {
    await ctx.dc.sendText(reply.accountId, reply.chatId, text);
    ctx.db.appendMessage({
      id: `dc:${reply.accountId}:${reply.chatId}:${ctx.now()}:reply`,
      companionId: state.companionId,
      role: "assistant",
      sender: ctx.db.getCompanion(state.companionId)?.name ?? "Companion",
      text,
      source: "deltachat",
      channel: "deltachat",
      cardTitle: "",
      tsMs: ctx.now(),
      peer: reply.peer,
    });
    ctx.notify();
  } catch (error) {
    ctx.log(`reply routing: ${errMessage(error)}`);
  }
}

export async function playbackDone(ctx: VoiceContext, event: EdgePlayback): Promise<void> {
  const state = ctx.terminal(event.terminal_id);
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
    await ctx.renderHome(state, "idle");
    ctx.notify();
    return;
  }
  const companion = ctx.db.getCompanion(state.companionId);
  if (state.reply !== undefined) {
    state.reply.armedUntil = ctx.now() + REPLY_CONTEXT_MS;
  }
  if (companion?.listening === true) await armListening(ctx, state);
  else await ctx.renderHome(state, "idle");
  ctx.notify();
}

export async function cancel(ctx: VoiceContext, terminalId: string, reason: string): Promise<void> {
  const state = ctx.terminal(terminalId);
  if (state === undefined) return;
  state.speculation?.handle.abort(new Error(reason));
  state.speculation = undefined;
  state.turn?.abort();
  state.turn = undefined;
  state.busy = false;
  state.speaking = false;
  state.listening = false;
  await ctx.renderHome(state, "idle");
  ctx.notify();
}

export async function armListening(
  ctx: VoiceContext,
  state: TerminalState,
  startRec = true,
): Promise<void> {
  if (ctx.closed()) return;
  const companion = ctx.db.getCompanion(state.companionId);
  if (state.listenTimer !== undefined) clearTimeout(state.listenTimer);
  state.listening = true;
  state.listeningSince = ctx.now();
  state.drawer = "";
  state.story = undefined;
  await ctx.renderHome(state, "listening");
  if (startRec) {
    try {
      await ctx.edge.recStart(state.id, {
        listenWindow: true,
        windowS: Math.round(LISTEN_WINDOW_MS / 1000),
        followUpGraceMs: FOLLOW_UP_GRACE_MS,
        companionName: companion?.name ?? "",
      });
    } catch (error) {
      ctx.log(`listen arm: ${errMessage(error)}`);
    }
  }
  state.listenTimer = setTimeout(() => {
    state.listenTimer = undefined;
    if (state.listening && !state.busy && !state.speaking) {
      state.listening = false;
      void ctx.renderHome(state, "idle").catch(() => undefined);
    }
  }, LISTEN_WINDOW_MS + 2000);
  state.listenTimer.unref();
  ctx.notify();
}

function audioTerminal(ctx: VoiceContext, companionId: string): TerminalState {
  const state = ctx.onlineTerminals(companionId)[0];
  if (state === undefined) throw new Error("terminal is offline");
  return state;
}

export async function audioMetronome(
  ctx: VoiceContext,
  companionId: string,
  bpm: number,
  beats: number,
  durationS: number,
): Promise<Record<string, unknown>> {
  const state = audioTerminal(ctx, companionId);
  await ctx.edge.stopAudio(state.id);
  const caption = `Metronome · ${bpm} BPM (${beats}/4)`;
  state.audio = "metronome";
  state.audioStreamId = undefined;
  state.speaking = true;
  state.listening = false;
  state.drawer = "";
  state.story = undefined;
  await ctx.renderHome(state, "speaking", caption);
  const streamId = await ctx.edge.metronome(state.id, bpm, beats, durationS);
  state.audioStreamId = streamId;
  ctx.notify();
  return { terminal_id: state.id, caption, bpm, beats, duration_s: durationS, stream_id: streamId };
}

export async function audioTune(
  ctx: VoiceContext,
  companionId: string,
  tempo: number,
  style: string,
  notes: TuneNote[],
  title?: string,
): Promise<Record<string, unknown>> {
  const state = audioTerminal(ctx, companionId);
  await ctx.edge.stopAudio(state.id);
  const caption = title === undefined || title === "" ? `Tune · ${style}` : `Tune · ${title} (${style})`;
  state.audio = "tune";
  state.audioStreamId = undefined;
  state.speaking = true;
  state.listening = false;
  state.drawer = "";
  state.story = undefined;
  await ctx.renderHome(state, "speaking", caption);
  const streamId = await ctx.edge.playTune(state.id, tempo, style, notes);
  state.audioStreamId = streamId;
  ctx.notify();
  return { terminal_id: state.id, caption, tempo, style, notes: notes.length, stream_id: streamId };
}

export async function audioIdentify(
  ctx: VoiceContext,
  companionId: string,
  seconds: number,
): Promise<SongMatch & Record<string, unknown>> {
  const state = audioTerminal(ctx, companionId);
  if (ctx.identifySong === undefined) throw new Error("song recognition is unavailable");
  const dir = mkdtempSync(join(tmpdir(), "lark-song-"));
  const wav = join(dir, "capture.wav");
  try {
    await ctx.edge.stopAudio(state.id);
    state.audio = "identify";
    state.audioStreamId = undefined;
    state.speaking = false;
    state.listening = true;
    state.drawer = "";
    state.story = undefined;
    await ctx.renderHome(state, "listening", "Listening to music...");
    const captured = new Promise<EdgeCaptureDone>((resolve, reject) => {
      let off: () => void = () => undefined;
      const timer = setTimeout(() => {
        off();
        reject(new Error("music capture timed out"));
      }, (seconds + 8) * 1000);
      off = ctx.edge.on("capture.done", (event) => {
        if (event.terminal_id !== state.id || event.path !== wav) return;
        clearTimeout(timer);
        off();
        resolve(event);
      });
    });
    await ctx.edge.captureStart(state.id, wav, seconds);
    const done = await captured;
    if (done.error !== undefined && done.error !== "") throw new Error(done.error);
    return { ...(await ctx.identifySong(wav)), terminal_id: state.id, seconds };
  } finally {
    state.audio = undefined;
    state.audioStreamId = undefined;
    state.listening = false;
    await ctx.renderHome(state, "idle", "").catch(() => undefined);
    rmSync(dir, { recursive: true, force: true });
    ctx.notify();
  }
}

export async function audioStop(ctx: VoiceContext, companionId: string): Promise<Record<string, unknown>> {
  const state = audioTerminal(ctx, companionId);
  const stopped = state.audio !== undefined;
  if (state.audio === "identify") await ctx.edge.captureStop(state.id).catch(() => undefined);
  state.audio = undefined;
  state.audioStreamId = undefined;
  state.listening = false;
  state.speaking = false;
  await ctx.edge.stopAudio(state.id);
  await ctx.renderHome(state, "idle", "");
  ctx.notify();
  return { terminal_id: state.id, stopped };
}

export async function sayNow(ctx: VoiceContext, companionId: string, text: string): Promise<void> {
  const state = ctx.onlineTerminals(companionId)[0];
  if (state === undefined) return;
  await speakPhrase(ctx, state.id, text);
}

export async function speakPhrase(ctx: VoiceContext, terminalId: string, text: string): Promise<void> {
  const state = ctx.terminal(terminalId);
  if (state === undefined) return;
  const companion = ctx.db.getCompanion(state.companionId);
  const stream = new SpeechStream(
    ctx.edge,
    terminalId,
    companion?.voice === "" ? "af_heart" : companion?.voice ?? "af_heart",
    ctx.nextStream(),
  );
  stream.push(text);
  await stream.end();
  state.speaking = true;
  state.caption = sanitizeSpeech(text).slice(0, 120);
  ctx.notify();
  await ctx.renderHome(state, "speaking", state.caption);
}

export async function setListening(
  ctx: VoiceContext,
  companionId: string,
  listening: boolean,
): Promise<void> {
  ctx.db.patchCompanion(companionId, { listening });
  for (const state of ctx.terminals()) {
    if (state.companionId !== companionId) continue;
    if (listening) await armListening(ctx, state);
    else {
      state.listening = false;
      await ctx.edge.recCancel(state.id);
      await ctx.renderHome(state, "idle");
    }
  }
  ctx.notify();
}

export async function notifyDcTurn(
  ctx: VoiceContext,
  turn: DcPeerTurn & { accountId?: number; chatId?: number },
): Promise<string | undefined> {
  const companion = ctx.db.getCompanion(turn.companionId);
  if (companion === undefined) return undefined;
  const state = ctx.onlineTerminals(turn.companionId)[0];
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
    await speakPhrase(ctx, state.id, `Message from ${turn.sender}: ${turn.text}`);
  }
  try {
    const result = await runTurn(ctx, turn.companionId, `${turn.sender} via Delta Chat: ${turn.text}`);
    return sanitizeSpeech(result.text);
  } catch (error) {
    ctx.log(`dc turn: ${errMessage(error)}`);
    return undefined;
  }
}

