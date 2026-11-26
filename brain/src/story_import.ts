// Convert plain library text into a valid linear Twee 3 story.

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
