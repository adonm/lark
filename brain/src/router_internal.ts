// State shared by the router facade and its voice/UI modules. Keeping the
// shapes here avoids a runtime import cycle between router.ts, router_ui.ts
// and router_voice.ts.
import type { EdgeTelemetry, EdgeTerminal } from "./edge_client.ts";
import type { Story, StoryValue } from "./stories.ts";
import type { DeltaCapture } from "./speech.ts";
import type { SpeculationHandle } from "./opencode.ts";

export interface StoryRun {
  id: string;
  story: Story;
  passage: number;
  flags: StoryValue[];
  title: string;
  body: string;
  choices: Array<{ label: string; target: number }>;
  page: number;
}

export interface ReplyContext {
  accountId: number;
  chatId: number;
  contact: string;
  /** Lowercased Delta Chat address; scopes history to this conversation. */
  peer: string;
  armedUntil: number;
}

export interface TerminalState {
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
  speculation?: { transcript: string; stream: DeltaCapture; handle: SpeculationHandle };
  turn?: AbortController;
}

export function statusOf(state: Pick<TerminalState, "speaking" | "busy" | "listening">): string {
  if (state.speaking) return "speaking";
  if (state.busy) return "thinking";
  if (state.listening) return "listening";
  return "idle";
}
