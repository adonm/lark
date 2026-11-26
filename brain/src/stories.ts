// Twee 3 subset used by Lark stories. This replaces the removed Go
// lark/internal/stories package (parse, compile, validate, eval, voice
// matching); a story authored against the v1 server parses identically here.
import { slugify } from "./util.ts";

export const MAX_PASSAGES = 64;
export const MAX_PASSAGE_TEXT = 600;
export const MAX_CHOICES = 3;
export const MAX_FLAGS = 64;

export type StoryValue = boolean | number;

export interface Choice {
  label: string;
  target: number;
}

export type Op =
  | { kind: "text"; text: string }
  | { kind: "choice"; text: string; target: number }
  | { kind: "set"; flag: number; value: StoryValue }
  | { kind: "if-not"; flag: number; target: number }
  | { kind: "jump"; target: number };

export interface Passage {
  name: string;
  bg: string;
  end: boolean;
  ops: Op[];
}

export interface Story {
  title: string;
  start: number;
  passages: Passage[];
  byName: Map<string, number>;
  flags: string[];
  flagIdx: Map<string, number>;
}

export interface EvalResult {
  text: string;
  choices: Choice[];
}

interface RawPassage {
  name: string;
  bg: string;
  end: boolean;
  lineNo: number;
  lines: Array<{ lineNo: number; text: string }>;
}

interface IfFrame {
  ifNotOp: number;
  jumpOp: number;
  hasElse: boolean;
  lineNo: number;
}

function isValidFlagName(name: string): boolean {
  if (name === "") return false;
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(name);
}

function containsHTML(value: string): boolean {
  for (let i = 0; i < value.length; i++) {
    if (value[i] !== "<") continue;
    if (i + 1 < value.length && value[i + 1] === "<") {
      i++;
      continue;
    }
    if (i + 1 < value.length) {
      const next = value[i + 1] ?? "";
      if (/[A-Za-z/!]/.test(next)) return true;
    }
  }
  return false;
}

function parsePassageHeader(
  line: string,
  lineNo: number,
): { name: string; bg: string; end: boolean } {
  const rest = line.slice(2).trim();
  if (rest === "") throw new Error(`line ${lineNo}: empty passage name`);
  let name: string;
  let bg = "";
  let end = false;
  const brace = rest.indexOf("{");
  if (brace >= 0) {
    name = rest.slice(0, brace).trim();
    const metaStr = rest.slice(brace).trim();
    let meta: unknown;
    try {
      meta = JSON.parse(metaStr);
    } catch {
      throw new Error(`line ${lineNo}: invalid passage metadata JSON`);
    }
    if (meta === null || typeof meta !== "object" || Array.isArray(meta)) {
      throw new Error(`line ${lineNo}: invalid passage metadata JSON`);
    }
    for (const [key, value] of Object.entries(meta)) {
      if (key === "bg") {
        if (typeof value !== "string") throw new Error(`line ${lineNo}: metadata bg must be a string`);
        bg = value.trim();
        if (bg.length > 160) throw new Error(`line ${lineNo}: metadata bg exceeds 160 characters`);
      } else if (key === "end") {
        if (typeof value !== "boolean") throw new Error(`line ${lineNo}: metadata end must be a boolean`);
        end = value;
      } else {
        throw new Error(`line ${lineNo}: unsupported metadata key ${JSON.stringify(key)}`);
      }
    }
  } else {
    name = rest;
  }
  if (name === "") throw new Error(`line ${lineNo}: empty passage name`);
  if (name.includes("[") || name.includes("]")) {
    throw new Error(`line ${lineNo}: invalid passage name ${JSON.stringify(name)}`);
  }
  return { name, bg, end };
}

function lexTwee(src: string): RawPassage[] {
  const normalized = src.replaceAll("\r\n", "\n");
  const lines = normalized.split("\n");
  const passages: RawPassage[] = [];
  let current: RawPassage | undefined;
  for (let i = 0; i < lines.length; i++) {
    const lineNo = i + 1;
    const line = lines[i] ?? "";
    if (line.startsWith("::")) {
      if (line.length < 3 || line[2] !== " ") {
        throw new Error(`line ${lineNo}: passage header must start with ":: "`);
      }
      const header = parsePassageHeader(line, lineNo);
      current = { ...header, lineNo, lines: [] };
      passages.push(current);
      continue;
    }
    if (current === undefined) {
      if (line.trim() === "") continue;
      throw new Error(`line ${lineNo}: content outside passage`);
    }
    current.lines.push({ lineNo, text: line });
  }
  return passages;
}

function collectStoryPassages(passages: RawPassage[]): {
  title: string;
  startName: string;
  storyRaw: RawPassage[];
} {
  let title = "";
  let startName = "";
  const storyRaw: RawPassage[] = [];
  const seen = new Set<string>();
  for (const passage of passages) {
    if (seen.has(passage.name)) {
      throw new Error(`line ${passage.lineNo}: duplicate passage ${JSON.stringify(passage.name)}`);
    }
    seen.add(passage.name);
    if (passage.name === "StoryTitle") {
      title = passage.lines.map((line) => line.text).join("\n").trim();
      if (title === "") throw new Error(`line ${passage.lineNo}: StoryTitle cannot be empty`);
    } else if (passage.name === "StoryData") {
      const body = passage.lines.map((line) => line.text).join("\n").trim();
      if (body !== "") {
        let data: unknown;
        try {
          data = JSON.parse(body);
        } catch {
          throw new Error(`line ${passage.lineNo}: invalid StoryData JSON`);
        }
        if (data !== null && typeof data === "object" && !Array.isArray(data)) {
          const start = (data as Record<string, unknown>).start;
          if (typeof start === "string") startName = start.trim();
        }
      }
    } else if (
      passage.name === "StoryIncludes" ||
      passage.name === "StoryStylesheet" ||
      passage.name === "StoryScript"
    ) {
      throw new Error(
        `line ${passage.lineNo}: unsupported Twee special passage ${JSON.stringify(passage.name)}`,
      );
    } else {
      storyRaw.push(passage);
    }
  }
  return { title, startName, storyRaw };
}

function internFlag(story: Story, name: string, lineNo: number): number {
  const existing = story.flagIdx.get(name);
  if (existing !== undefined) return existing;
  if (story.flags.length >= MAX_FLAGS) throw new Error(`line ${lineNo}: too many flags (max ${MAX_FLAGS})`);
  const index = story.flags.length;
  story.flags.push(name);
  story.flagIdx.set(name, index);
  return index;
}

function linkOp(story: Story, body: string, lineNo: number): Op {
  if (body === "") throw new Error(`line ${lineNo}: empty link [[]]`);
  let label: string;
  let target: string;
  const arrow = body.indexOf("->");
  if (arrow >= 0) {
    label = body.slice(0, arrow).trim();
    target = body.slice(arrow + 2).trim();
  } else {
    const pipe = body.indexOf("|");
    if (pipe >= 0) {
      label = body.slice(0, pipe).trim();
      target = body.slice(pipe + 1).trim();
    } else {
      label = body;
      target = body;
    }
  }
  if (label === "" || target === "") throw new Error(`line ${lineNo}: invalid link [[${body}]]`);
  const targetIdx = story.byName.get(target);
  if (targetIdx === undefined) {
    throw new Error(`line ${lineNo}: link target ${JSON.stringify(target)} does not exist`);
  }
  return { kind: "choice", text: label, target: targetIdx };
}

function macroOps(
  story: Story,
  body: string,
  ops: Op[],
  stack: IfFrame[],
  lineNo: number,
): void {
  if (body.startsWith("set ")) {
    const rest = body.slice(4).trim();
    if (!rest.startsWith("$")) throw new Error(`line ${lineNo}: <<set>> variable must start with $`);
    const toIdx = rest.indexOf(" to ");
    if (toIdx < 0) {
      throw new Error(`line ${lineNo}: <<set>> must use 'to' (e.g. <<set $flag to true>>)`);
    }
    const flagName = rest.slice(1, toIdx).trim();
    if (!isValidFlagName(flagName)) {
      throw new Error(`line ${lineNo}: invalid flag name ${JSON.stringify(flagName)}`);
    }
    const value = rest.slice(toIdx + 4).trim();
    let parsed: StoryValue;
    if (value === "true") parsed = true;
    else if (value === "false") parsed = false;
    else {
      const numberValue = Number(value);
      if (value === "" || Number.isNaN(numberValue)) {
        throw new Error(`line ${lineNo}: <<set>> value ${JSON.stringify(value)} must be true, false, or a number`);
      }
      parsed = numberValue;
    }
    ops.push({ kind: "set", flag: internFlag(story, flagName, lineNo), value: parsed });
    return;
  }

  if (body.startsWith("if ")) {
    const rest = body.slice(3).trim();
    if (!rest.startsWith("$")) throw new Error(`line ${lineNo}: <<if>> variable must start with $`);
    const flagName = rest.slice(1).trim();
    if (!isValidFlagName(flagName)) {
      throw new Error(`line ${lineNo}: invalid <<if>> condition ${JSON.stringify(rest)}`);
    }
    const opIndex = ops.length;
    ops.push({ kind: "if-not", flag: internFlag(story, flagName, lineNo), target: 0 });
    stack.push({ ifNotOp: opIndex, jumpOp: -1, hasElse: false, lineNo });
    return;
  }

  if (body === "else") {
    const top = stack.at(-1);
    if (top === undefined) throw new Error(`line ${lineNo}: <<else>> without matching <<if>>`);
    if (top.hasElse) throw new Error(`line ${lineNo}: duplicate <<else>>`);
    top.hasElse = true;
    top.jumpOp = ops.length;
    ops.push({ kind: "jump", target: 0 });
    const ifNot = ops[top.ifNotOp];
    if (ifNot !== undefined && ifNot.kind === "if-not") ifNot.target = ops.length;
    return;
  }

  if (body === "endif") {
    const top = stack.pop();
    if (top === undefined) throw new Error(`line ${lineNo}: <<endif>> without matching <<if>>`);
    const index = top.hasElse ? top.jumpOp : top.ifNotOp;
    const target = ops[index];
    if (target !== undefined && (target.kind === "if-not" || target.kind === "jump")) {
      target.target = ops.length;
    }
    return;
  }

  throw new Error(`line ${lineNo}: unsupported macro <<${body}>>`);
}

function compilePassage(
  story: Story,
  passage: RawPassage,
): { ops: Op[]; linkCount: number; textLen: number } {
  const ops: Op[] = [];
  const stack: IfFrame[] = [];
  let linkCount = 0;
  let textLen = 0;

  for (const raw of passage.lines) {
    if (containsHTML(raw.text)) throw new Error(`line ${raw.lineNo}: HTML tags are not allowed`);
    const line = raw.text;
    let emittedContentOnLine = false;
    let pos = 0;
    while (pos < line.length) {
      const nextMacro = line.indexOf("<<", pos);
      const nextLink = line.indexOf("[[", pos);
      let nextIdx = -1;
      let kind: "macro" | "link" | undefined;
      if (nextMacro >= 0 && (nextLink < 0 || nextMacro < nextLink)) {
        nextIdx = nextMacro;
        kind = "macro";
      } else if (nextLink >= 0) {
        nextIdx = nextLink;
        kind = "link";
      }
      if (nextIdx < 0) {
        const chunk = line.slice(pos);
        if (chunk !== "") {
          ops.push({ kind: "text", text: chunk });
          textLen += chunk.length;
          emittedContentOnLine = true;
        }
        break;
      }
      if (nextIdx > pos) {
        const chunk = line.slice(pos, nextIdx);
        ops.push({ kind: "text", text: chunk });
        textLen += chunk.length;
        emittedContentOnLine = true;
      }
      if (kind === "macro") {
        const end = line.indexOf(">>", nextIdx + 2);
        if (end < 0) throw new Error(`line ${raw.lineNo}: unclosed macro <<`);
        const body = line.slice(nextIdx + 2, end).trim();
        pos = end + 2;
        macroOps(story, body, ops, stack, raw.lineNo);
      } else {
        const end = line.indexOf("]]", nextIdx + 2);
        if (end < 0) throw new Error(`line ${raw.lineNo}: unclosed link [[`);
        const body = line.slice(nextIdx + 2, end).trim();
        pos = end + 2;
        ops.push(linkOp(story, body, raw.lineNo));
        linkCount++;
      }
    }
    if (emittedContentOnLine || passage.lines.length > 1) {
      if (emittedContentOnLine) {
        ops.push({ kind: "text", text: "\n" });
        textLen++;
      } else if (line.trim() === "" && textLen > 0) {
        ops.push({ kind: "text", text: "\n" });
        textLen++;
      }
    }
  }

  const open = stack.at(-1);
  if (open !== undefined) {
    throw new Error(`line ${open.lineNo}: unclosed <<if>> in passage ${JSON.stringify(passage.name)}`);
  }
  return { ops, linkCount, textLen };
}

function validateReachable(story: Story): void {
  const visited = new Array<boolean>(story.passages.length).fill(false);
  const queue: number[] = [story.start];
  visited[story.start] = true;
  while (queue.length > 0) {
    const currentIndex = queue.shift();
    if (currentIndex === undefined) break;
    const current = story.passages[currentIndex];
    if (current === undefined) continue;
    for (const op of current.ops) {
      if (op.kind === "choice" && !visited[op.target]) {
        visited[op.target] = true;
        queue.push(op.target);
      }
    }
  }
  for (let i = 0; i < story.passages.length; i++) {
    const passage = story.passages[i];
    if (passage !== undefined && !visited[i]) {
      const start = story.passages[story.start];
      throw new Error(
        `passage ${JSON.stringify(passage.name)} is unreachable from ${JSON.stringify(start?.name ?? "")}`,
      );
    }
  }
}

export function parseTwee(src: string): Story {
  const rawPassages = lexTwee(src);
  const { title, startName, storyRaw } = collectStoryPassages(rawPassages);
  if (title === "") throw new Error("missing StoryTitle passage");
  if (storyRaw.length === 0) throw new Error("story has no passages");
  if (storyRaw.length > MAX_PASSAGES) {
    throw new Error(`story has ${storyRaw.length} passages (max ${MAX_PASSAGES})`);
  }

  const story: Story = {
    title,
    start: 0,
    passages: storyRaw.map((passage) => ({ name: passage.name, bg: passage.bg, end: passage.end, ops: [] })),
    byName: new Map(storyRaw.map((passage, index) => [passage.name, index])),
    flags: [],
    flagIdx: new Map(),
  };

  if (startName !== "") {
    const index = story.byName.get(startName);
    if (index === undefined) throw new Error(`StoryData start passage ${JSON.stringify(startName)} does not exist`);
    story.start = index;
  } else {
    story.start = story.byName.get("Start") ?? 0;
  }

  for (let index = 0; index < storyRaw.length; index++) {
    const raw = storyRaw[index];
    const target = story.passages[index];
    if (raw === undefined || target === undefined) continue;
    const { ops, linkCount, textLen } = compilePassage(story, raw);
    if (linkCount > MAX_CHOICES) {
      throw new Error(`passage ${JSON.stringify(raw.name)} has ${linkCount} links (max ${MAX_CHOICES})`);
    }
    if (textLen > MAX_PASSAGE_TEXT) {
      throw new Error(
        `passage ${JSON.stringify(raw.name)} text is ${textLen} characters (max ${MAX_PASSAGE_TEXT})`,
      );
    }
    if (raw.end && linkCount > 0) {
      throw new Error(`ending passage ${JSON.stringify(raw.name)} must not have links`);
    }
    if (!raw.end && linkCount === 0) {
      throw new Error(
        `passage ${JSON.stringify(raw.name)} is a dead end (add a link or {"end": true})`,
      );
    }
    target.ops = ops;
  }

  validateReachable(story);
  return story;
}

function truthy(value: StoryValue | undefined): boolean {
  if (value === undefined) return false;
  return typeof value === "number" ? value !== 0 : value;
}

export function evalPassage(
  story: Story,
  passageIndex: number,
  flags: StoryValue[],
  applySets = true,
): EvalResult {
  const passage = story.passages[passageIndex];
  if (passage === undefined) return { text: "", choices: [] };
  const chunks: string[] = [];
  const choices: Choice[] = [];
  let pc = 0;
  while (pc < passage.ops.length) {
    const op = passage.ops[pc];
    if (op === undefined) break;
    switch (op.kind) {
      case "text":
        chunks.push(op.text);
        pc++;
        break;
      case "choice":
        if (choices.length < MAX_CHOICES) choices.push({ label: op.text, target: op.target });
        pc++;
        break;
      case "set":
        if (applySets) flags[op.flag] = op.value;
        pc++;
        break;
      case "if-not":
        pc = truthy(flags[op.flag]) ? pc + 1 : op.target;
        break;
      case "jump":
        pc = op.target;
        break;
    }
  }
  return { text: chunks.join("").trim(), choices };
}

export function flagsSlice(story: Story, values?: Readonly<Record<string, StoryValue>>): StoryValue[] {
  const flags = new Array<StoryValue>(story.flags.length).fill(false);
  if (values === undefined) return flags;
  for (const [name, index] of story.flagIdx) {
    const value = values[name];
    if (value !== undefined) flags[index] = value;
  }
  return flags;
}

export function flagsMap(story: Story, values: readonly StoryValue[]): Record<string, StoryValue> {
  const out: Record<string, StoryValue> = {};
  for (let i = 0; i < story.flags.length; i++) {
    const name = story.flags[i];
    const value = values[i];
    if (name === undefined || value === undefined) continue;
    if (truthy(value) || typeof value === "number") out[name] = value;
  }
  return out;
}

export function normalizeSpeechTokens(value: string): string[] {
  return value
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s]/gu, " ")
    .split(/\s+/)
    .filter(Boolean);
}

const STOP_TOKENS = new Set(["the", "a", "an", "to", "and", "or", "of", "in", "on", "at", "for", "with"]);

export function filterStopTokens(tokens: readonly string[]): string[] {
  return tokens.filter((token) => !STOP_TOKENS.has(token));
}

/** Match spoken text to one of at most three labels without an LLM. */
export function matchChoice(spoken: string, choices: readonly Choice[]): { index: number; matched: boolean } {
  if (choices.length === 0) return { index: 0, matched: false };
  const tokens = normalizeSpeechTokens(spoken);
  if (tokens.length === 0) return { index: 0, matched: false };

  let bestIdx = -1;
  let bestScore = 0;
  let secondScore = 0;
  const spokenJoined = tokens.join(" ");
  for (let i = 0; i < choices.length; i++) {
    const choice = choices[i];
    if (choice === undefined) continue;
    let choiceTokens = filterStopTokens(normalizeSpeechTokens(choice.label));
    if (choiceTokens.length === 0) choiceTokens = normalizeSpeechTokens(choice.label);
    if (choiceTokens.length === 0) continue;
    let hits = 0;
    for (const token of choiceTokens) if (tokens.includes(token)) hits++;
    if (hits === 0) continue;
    let score = hits / choiceTokens.length;
    if (spokenJoined.includes(choiceTokens.join(" "))) score += 0.5;
    if (score > bestScore) {
      secondScore = bestScore;
      bestScore = score;
      bestIdx = i;
    } else if (score > secondScore) {
      secondScore = score;
    }
  }
  if (bestIdx >= 0 && bestScore >= 0.5 && bestScore > secondScore) {
    return { index: bestIdx, matched: true };
  }

  let ordinal = -1;
  for (const token of tokens) {
    switch (token) {
      case "first":
      case "1st":
      case "one":
      case "1":
        ordinal = 0;
        break;
      case "second":
      case "2nd":
      case "two":
      case "2":
        ordinal = 1;
        break;
      case "third":
      case "3rd":
      case "three":
      case "3":
        ordinal = 2;
        break;
      case "last":
        ordinal = choices.length - 1;
        break;
    }
    if (ordinal >= 0) break;
  }
  if (ordinal >= 0 && ordinal < choices.length) return { index: ordinal, matched: true };
  return { index: 0, matched: false };
}

export function slugFromTitle(title: string): string {
  return slugify(title.trim().toLowerCase(), { max: 40, fallback: "story" });
}

// ---- import -------------------------------------------------------------

const IMPORT_PASSAGE_TARGET = 540;
const IMPORT_PASSAGE_MAX = 600;

export interface ImportOptions {
  /** Cap the number of generated passages (Go's importer uses 60). */
  maxPassages?: number;
}

function cleanImportedText(text: string): string {
  return text
    .replaceAll("\r\n", "\n")
    .replace(/<[^>]*>/g, " ")
    .replaceAll("[[", "[")
    .replaceAll("]]", "]")
    .replaceAll("<<", "‹")
    .replaceAll(">>", "›")
    .replace(/(^|\n)::/g, "$1: ")
    .replace(/[ \t]+/g, " ")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function splitPassages(text: string, maxPassages: number): string[] {
  const words = text.split(/\s+/).filter(Boolean);
  const chunks: string[] = [];
  let current: string[] = [];
  let length = 0;
  for (const word of words) {
    const extra = current.length === 0 ? word.length : word.length + 1;
    if (length + extra > IMPORT_PASSAGE_TARGET && current.length > 0) {
      chunks.push(current.join(" "));
      if (chunks.length >= maxPassages) return chunks;
      current = [];
      length = 0;
    }
    current.push(word);
    length += extra;
  }
  if (current.length > 0) chunks.push(current.join(" "));
  return chunks.slice(0, maxPassages);
}

/**
 * Convert a Gutenberg or library text into a valid, linear Twee 3 story. The
 * result always parses: one link per passage, a reachable chain, and an
 * explicit ending.
 */
export function tweeFromText(title: string, text: string, options: ImportOptions = {}): string {
  const maxPassages = Math.max(1, Math.min(60, Math.trunc(options.maxPassages ?? 60)));
  const cleanTitle = title.trim() || "Imported Story";
  const cleaned = cleanImportedText(text);
  let chunks = splitPassages(cleaned, maxPassages);
  if (chunks.length === 0) chunks = ["The end."];
  // Links are part of a passage's 600-character budget.
  const trimmed = chunks.map((chunk) => (chunk.length > IMPORT_PASSAGE_MAX ? chunk.slice(0, IMPORT_PASSAGE_MAX) : chunk));
  const out: string[] = [":: StoryTitle", cleanTitle, "", ":: StoryData", JSON.stringify({ start: "Start" }), ""];
  for (let i = 0; i < trimmed.length; i++) {
    const name = i === 0 ? "Start" : `Part ${i + 1}`;
    const last = i === trimmed.length - 1;
    out.push(`:: ${name}${last ? ' {"end": true}' : ""}`);
    out.push(trimmed[i] ?? "");
    if (!last) out.push(`[[Continue->Part ${i + 2}]]`);
    out.push("");
  }
  return `${out.join("\n").trim()}\n`;
}
