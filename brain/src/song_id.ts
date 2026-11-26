// Key-free song recognition. shazamio-core (MIT, WASM) builds the Shazam
// fingerprint locally; a single request to Shazam's public discovery endpoint
// returns the match. No API key, native binary, or audio is kept after the
// call. The runner is injectable so unit tests never touch the network.
import { randomUUID } from "node:crypto";
import { readFileSync, rmSync } from "node:fs";
import { recognizeBytes } from "shazamio-core";

export interface SongMatch {
  matched: boolean;
  title?: string;
  artist?: string;
  album?: string;
  subtitle?: string;
}

export type SongRunner = (wavPath: string, signal: AbortSignal) => Promise<SongMatch>;

export interface IdentifyOptions {
  runner?: SongRunner;
  timeoutMs?: number;
}

const ENDPOINT = "https://amp.shazam.com/discovery/v5/en/US/android/-/tag";
const TIMEOUT_MS = 12_000;
const MAX_SECONDS = 8;

function text(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

function object(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : undefined;
}

/** Map the Shazam discovery document to the small shape the agent needs. */
export function parseShazamResponse(raw: unknown): SongMatch {
  const root = object(raw);
  if (root === undefined) return { matched: false };
  const matches = Array.isArray(root.matches) ? root.matches : [];
  const first = object(matches[0]);
  const track = object(first?.track) ?? object(root.track);
  if (track === undefined) return { matched: false };
  const title = text(track.title);
  if (title === undefined) return { matched: false };
  const subtitle = text(track.subtitle);
  const sections = Array.isArray(track.sections) ? track.sections : [];
  let album: string | undefined;
  for (const entry of sections) {
    const section = object(entry);
    if (section?.type !== "SONG" || !Array.isArray(section.metadata)) continue;
    for (const item of section.metadata) {
      const meta = object(item);
      if (meta?.title === "Album") album = text(meta.text);
    }
  }
  return {
    matched: true,
    title,
    ...(subtitle === undefined ? {} : { artist: subtitle, subtitle }),
    ...(album === undefined ? {} : { album }),
  };
}

/** Default runner: local fingerprint + one bounded HTTPS request. */
export async function recognizeSongFile(wavPath: string, signal: AbortSignal): Promise<SongMatch> {
  const signatures = recognizeBytes(new Uint8Array(readFileSync(wavPath)), 0, MAX_SECONDS);
  try {
    if (signatures.length === 0) return { matched: false };
    const signature = signatures[0];
    if (signature === undefined) return { matched: false };
    const url = new URL(`${ENDPOINT}/${randomUUID()}/${randomUUID()}`);
    for (const [key, value] of Object.entries({
      sync: "true",
      webv3: "true",
      sampling: "true",
      connected: "",
      shazamapiversion: "v3",
      sharehub: "true",
      video: "v3",
    })) {
      url.searchParams.append(key, value);
    }
    const response = await fetch(url, {
      method: "POST",
      signal,
      headers: {
        "X-Shazam-Platform": "IPHONE",
        "X-Shazam-AppVersion": "14.1.0",
        Accept: "*/*",
        "Content-Type": "application/json",
        "Accept-Language": "en",
        "User-Agent": "Lark/2 song-id",
      },
      body: JSON.stringify({
        timezone: "UTC",
        signature: { uri: signature.uri, samplems: signature.samplems },
        timestamp: Date.now(),
        context: {},
        geolocation: {},
      }),
    });
    if (!response.ok) throw new Error(`song recognition failed: HTTP ${response.status}`);
    return parseShazamResponse(await response.json());
  } finally {
    for (const signature of signatures) signature.free();
  }
}

/**
 * Recognize a temporary capture WAV. The file is deleted in `finally`, no
 * matter whether recognition matched, missed, failed, or timed out.
 */
export async function identifySong(wavPath: string, options: IdentifyOptions = {}): Promise<SongMatch> {
  const runner = options.runner ?? recognizeSongFile;
  const timeoutMs = options.timeoutMs ?? TIMEOUT_MS;
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => {
      controller.abort();
      reject(new Error("song recognition timed out"));
    }, timeoutMs);
  });
  try {
    return await Promise.race([runner(wavPath, controller.signal), timeout]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
    controller.abort();
    rmSync(wavPath, { force: true });
  }
}

/** `lark doctor` uses this to prove the WASM recognizer loaded. */
export function songRecognizerReady(): boolean {
  return typeof recognizeBytes === "function";
}
