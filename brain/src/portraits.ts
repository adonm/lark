// Nano Banana portrait sheets: one 1024×1024 expression sheet, normalized by
// the Go edge into four pixel poses, plus a matching background. Generation is
// single-flight per companion; the directory rename and database update happen
// only after the edge has accepted the sheet, and superseded dirs are removed.
import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { LarkDb } from "./db.ts";
import type { EdgeClient } from "./edge_client.ts";
import { errMessage } from "./util.ts";

export const PORTRAIT_MODEL = "gemini-nano-banana-2.1";
export const PORTRAIT_ENDPOINT = `https://opencode.ai/inference/google/v1beta/models/${PORTRAIT_MODEL}:generateContent`;
export const PORTRAIT_MAX_BYTES = 8 << 20;
export const PORTRAIT_SIDE = 1024;
export const PORTRAIT_READY_NOTICE = "Ta-da! My new look is ready!";
const PORTRAIT_ID = /^[0-9a-f]{32}$/;

export interface PortraitSpec {
  prompt: string;
  form?: string;
  recolor?: string;
  style?: "storybook" | "pixel";
  edit?: boolean;
}

export type PortraitGenerator = (prompt: string, source?: Buffer) => Promise<Buffer>;

export interface PortraitManagerOptions {
  db: LarkDb;
  edge: Pick<EdgeClient, "normalizePortrait">;
  dataDir: string;
  generate?: PortraitGenerator;
  now?: () => number;
  log?: (line: string) => void;
  /** Refresh every attached terminal for this companion. */
  onReady?: (companionId: string, portraitId: string) => void | Promise<void>;
  /** Speak the ready notice through the companion's live terminal. */
  announce?: (companionId: string, text: string) => void | Promise<void>;
}

const ART_STORYBOOK =
  "Cozy hand-painted gouache meets soft storybook vector: crisp hue-shifted cocoa, pine, plum or navy outlines (not black), slightly warm top highlights, softly colored base shadows, simple matte pastel shapes, glossy eyes with cream catchlights.";
const ART_PIXEL =
  "Use crisp 64×64-style pixel clusters with hue-shifted colored outlines, warm highlights and stepped shadows for a cozy Stardew Valley or GBA look. Keep a consistent square pixel grid at the same scale in all four poses, with no antialiasing or dithering.";

export function portraitPrompt(spec: PortraitSpec): string {
  const pixel = spec.style === "pixel";
  const style = pixel ? "indie-game pixel art" : "Lark Storybook Illustration";
  const art = pixel ? ART_PIXEL : ART_STORYBOOK;
  let prompt = `Create one square 1024×1024 ${style} expression sheet, exactly four equal quadrants, no text, labels, borders or gridlines.
Top-left idle: gentle closed-mouth smile. Top-right blink: both eyes closed, same smile. Bottom-left talk: open mouth, friendly eyes. Bottom-right think: eyes glance up, thoughtful mouth.
Draw the very same front-facing character, head and rounded shoulders, at the exact same scale, silhouette, position and line weight in every quadrant. Only the expression changes. Keep each silhouette well within its quadrant; maintain generous circular margin with nothing touching an edge.
${art} No extra props except requested accessories, no scene, no cast shadow, no texture or gradient on the background. Use the same solid flat background across the entire sheet. Output a PNG image.`;
  if (spec.edit === true) {
    prompt +=
      "\nUse the attached sheet as a reference. Keep character identity, silhouette, colors, expression layout and placement unchanged except for the requested change.";
  }
  if (spec.form !== undefined && spec.form.trim() !== "") prompt += `\nCharacter form: ${spec.form.trim()}`;
  if (spec.recolor !== undefined && spec.recolor.trim() !== "") prompt += `\nRecolor instruction: ${spec.recolor.trim()}`;
  if (spec.prompt.trim() !== "") prompt += `\nCharacter art direction: ${spec.prompt.trim()}`;
  return prompt;
}

export function backgroundPrompt(form: string, vibe: string): string {
  const scene = form.trim() === "" ? "a cozy sunlit window sill with potted plants" : form.trim();
  let prompt = `Create a cozy indie-game pixel art background scene (Stardew Valley or GBA style) that fits the habitat and vibe of: ${scene}.`;
  if (vibe.trim() !== "") prompt += ` Vibe: ${vibe.trim()}.`;
  return `${prompt} Use crisp pixel clusters, soft warm lighting, and a harmonious limited palette. No characters, no creatures, no text, no UI, no borders, no frames. Keep the center-top area slightly calmer so a foreground character stands out clearly. Output a PNG image.`;
}

/** Read PNG dimensions from the IHDR chunk without decoding pixels. */
export function pngSize(data: Buffer): { width: number; height: number } | undefined {
  if (data.length < 24) return undefined;
  if (data[0] !== 0x89 || data[1] !== 0x50 || data[2] !== 0x4e || data[3] !== 0x47) return undefined;
  if (data.toString("ascii", 12, 16) !== "IHDR") return undefined;
  return { width: data.readUInt32BE(16), height: data.readUInt32BE(20) };
}

export function defaultPortraitGenerator(endpoint = process.env.LARK_IMAGE_ENDPOINT ?? PORTRAIT_ENDPOINT): PortraitGenerator | undefined {
  const key = process.env.LARK_IMAGE_API_KEY ?? process.env.OPENCODE_API_KEY;
  if (key === undefined || key === "") return undefined;
  return async (prompt, source) => {
    const parts: Array<{ text?: string; inlineData?: { mimeType: string; data: string } }> = [{ text: prompt }];
    if (source !== undefined && source.length > 0) {
      parts.push({ inlineData: { mimeType: "image/png", data: source.toString("base64") } });
    }
    const response = await fetch(endpoint, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${key}` },
      body: JSON.stringify({
        contents: [{ parts }],
        generationConfig: {
          candidateCount: 1,
          responseModalities: ["IMAGE"],
          imageConfig: { aspectRatio: "1:1", imageSize: "1K" },
        },
      }),
      signal: AbortSignal.timeout(65_000),
    });
    if (response.status !== 200) throw new Error(`image service returned HTTP ${response.status}`);
    const raw = await response.text();
    const parsed = JSON.parse(raw) as {
      candidates?: Array<{ content?: { parts?: Array<{ inlineData?: { mimeType?: string; data?: string } }> } }>;
    };
    for (const candidate of parsed.candidates ?? []) {
      for (const part of candidate.content?.parts ?? []) {
        const data = part.inlineData?.data;
        if (data === undefined || data === "") continue;
        const bytes = Buffer.from(data, "base64");
        if (pngSize(bytes) !== undefined) return bytes;
        if (bytes.length > 0) return bytes;
      }
    }
    throw new Error("image response did not contain a portrait sheet");
  };
}

export class PortraitError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PortraitError";
  }
}

export class PortraitManager {
  readonly #db: LarkDb;
  readonly #edge: Pick<EdgeClient, "normalizePortrait">;
  readonly #dir: string;
  readonly #generate: PortraitGenerator | undefined;
  readonly #log: (line: string) => void;
  readonly #onReady: PortraitManagerOptions["onReady"];
  readonly #announce: PortraitManagerOptions["announce"];
  readonly #inflight = new Map<string, Promise<string>>();

  constructor(options: PortraitManagerOptions) {
    this.#db = options.db;
    this.#edge = options.edge;
    this.#dir = join(options.dataDir, "portraits");
    this.#generate = options.generate ?? defaultPortraitGenerator();
    this.#log = options.log ?? (() => undefined);
    this.#onReady = options.onReady;
    this.#announce = options.announce;
  }

  get available(): boolean {
    return this.#generate !== undefined;
  }

  dirFor(portraitId: string): string | undefined {
    if (!PORTRAIT_ID.test(portraitId)) return undefined;
    return join(this.#dir, portraitId);
  }

  request(companionId: string, spec: PortraitSpec): Promise<string> {
    const running = this.#inflight.get(companionId);
    if (running !== undefined) return running;
    const job = this.#run(companionId, spec).finally(() => {
      this.#inflight.delete(companionId);
    });
    this.#inflight.set(companionId, job);
    return job;
  }

  busy(companionId: string): boolean {
    return this.#inflight.has(companionId);
  }

  async #run(companionId: string, spec: PortraitSpec): Promise<string> {
    const generate = this.#generate;
    if (generate === undefined) {
      throw new PortraitError("Portrait generation unavailable; set LARK_IMAGE_API_KEY");
    }
    const companion = this.#db.getCompanion(companionId);
    if (companion === undefined) throw new PortraitError(`companion ${companionId} not found`);
    if (spec.prompt.trim() === "") throw new PortraitError("portrait prompt is required");

    let source: Buffer | undefined;
    if (spec.edit === true) {
      const dir = this.dirFor(companion.portraitId);
      if (dir === undefined || !existsSync(join(dir, "sheet.png"))) {
        throw new PortraitError("current companion has no local portrait sheet");
      }
      source = readFileSync(join(dir, "sheet.png"));
    }

    const [sheet, background] = await Promise.all([
      generate(portraitPrompt(spec), source),
      generate(backgroundPrompt(spec.form ?? companion.form, spec.prompt), undefined).catch(() => undefined),
    ]);
    if (sheet.length > PORTRAIT_MAX_BYTES) throw new PortraitError("portrait sheet exceeds 8 MiB");
    const size = pngSize(sheet);
    if (size === undefined || size.width !== PORTRAIT_SIDE || size.height !== PORTRAIT_SIDE) {
      throw new PortraitError("expected a 1024×1024 PNG portrait sheet");
    }

    mkdirSync(this.#dir, { recursive: true, mode: 0o700 });
    const id = randomBytes(16).toString("hex");
    const temp = join(this.#dir, `.tmp-${id}`);
    const target = join(this.#dir, id);
    mkdirSync(temp, { recursive: true, mode: 0o700 });
    try {
      writeFileSync(join(temp, "sheet.png"), sheet, { mode: 0o600 });
      if (background !== undefined && pngSize(background) !== undefined) {
        writeFileSync(join(temp, "bg.png"), background, { mode: 0o600 });
      }
      // The edge normalizes and writes the four pixel poses into temp before
      // the directory appears under its final id.
      await this.#edge.normalizePortrait(temp);
      renameSync(temp, target);
    } catch (error) {
      rmSync(temp, { recursive: true, force: true });
      throw error;
    }

    const previous = companion.portraitId;
    this.#db.patchCompanion(companionId, { portraitId: id });
    try {
      await this.#onReady?.(companionId, id);
    } catch (error) {
      this.#log(`portrait refresh: ${errMessage(error)}`);
    }
    this.#cleanup(previous, id);
    try {
      await this.#announce?.(companionId, PORTRAIT_READY_NOTICE);
    } catch (error) {
      this.#log(`portrait announce: ${errMessage(error)}`);
    }
    return id;
  }

  #cleanup(_previous: string, current: string): void {
    if (!existsSync(this.#dir)) return;
    for (const entry of readdirSync(this.#dir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      // Only completed portrait directories are removed; temp dirs are owned
      // by an in-flight request and clean themselves up on failure.
      if (entry.name === current || !PORTRAIT_ID.test(entry.name)) continue;
      rmSync(join(this.#dir, entry.name), { recursive: true, force: true });
    }
  }
}
