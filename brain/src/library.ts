// Read-only access to the offline content library and the disk-budget planner.
// Each source is one SQLite FTS5 database built by the Go library pipeline:
// `pages`/`pages_fts` for Wikipedia packs and `books`/`books_fts` for
// Project Gutenberg.
import { DatabaseSync } from "node:sqlite";
import { existsSync, readFileSync, readdirSync, statSync, statfsSync } from "node:fs";
import { dirname, join } from "node:path";

export const READ_CHUNK = 3200;
export const READ_MAX = 4000;
export const SNIPPET_WIDTH = 240;
const IMPORT_TEXT_MAX = 36 << 10;
const MAX_TEXT = 24 << 10;

export type SourceName = "simple" | "enwiki" | "gutenberg";
export type SourceChoice = SourceName | "auto";

export interface SourceMeta {
  name: SourceName;
  db: string;
  label: string;
  weight: number;
}

export const SOURCES: Record<SourceName, SourceMeta> = {
  simple: { name: "simple", db: "simple.db", label: "Simple English Wikipedia", weight: 2 },
  enwiki: { name: "enwiki", db: "enwiki.db", label: "English Wikipedia", weight: 1 },
  gutenberg: { name: "gutenberg", db: "gutenberg.db", label: "Project Gutenberg", weight: 1.2 },
};

/** Search order for "auto"; the planner uses its own disk order. */
export const SOURCE_ORDER: readonly SourceName[] = ["simple", "enwiki", "gutenberg"];

export class LibraryNotFoundError extends Error {
  constructor(message = "library text not found") {
    super(message);
    this.name = "LibraryNotFoundError";
  }
}

export interface Hit {
  source: SourceName;
  title: string;
  ref: string;
  snippet: string;
  rank: number;
}

export interface SearchResult {
  hits: Hit[];
  available: SourceName[];
}

export interface ReadResult {
  source: SourceName;
  title: string;
  ref: string;
  section: number;
  sections: number;
  text: string;
  chars: number;
  totalChars: number;
  nextSection?: number;
}

export interface ListResult {
  source: SourceName;
  available: boolean;
  total: number;
  items: Array<{ ref: string; title: string; author?: string }>;
}

const STOP_WORDS = new Set([
  "a", "an", "and", "are", "as", "at", "be", "by", "for", "from", "how", "in", "is", "it",
  "of", "on", "or", "that", "the", "this", "to", "was", "what", "when", "where", "which",
  "who", "why", "with",
]);

const WORD_RE = /[\p{L}\p{N}]+/gu;

export function ftsQuery(query: string): string {
  const words = query.toLowerCase().match(WORD_RE) ?? [];
  const kept = words.filter((word) => !STOP_WORDS.has(word));
  const terms = kept.length === 0 ? words : kept;
  return terms.map((word) => `"${word.replaceAll('"', "")}"`).join(" AND ");
}

export function ftsQueryOr(query: string): string {
  return ftsQuery(query).split(" AND ").join(" OR ");
}

function previewText(text: string, words: readonly string[]): string {
  const lower = text.toLowerCase();
  let best = -1;
  for (const word of words) {
    const index = lower.indexOf(word);
    if (index >= 0 && (best < 0 || index < best)) best = index;
  }
  if (best < 0) best = 0;
  let start = Math.max(0, best - 80);
  let end = Math.min(text.length, start + SNIPPET_WIDTH);
  while (start > 0 && text[start - 1] !== " ") start--;
  while (end < text.length && text[end] !== " ") end++;
  const snippet = text.slice(start, end).split(/\s+/).filter(Boolean).join(" ");
  return end < text.length ? `${snippet}…` : snippet;
}

interface WikiCandidate {
  id: number;
  title: string;
  rank: number;
}

function searchWiki(db: DatabaseSync, query: string, limit: number, weight: number): Hit[] {
  const run = (match: string): Hit[] => searchWikiMatch(db, query, match, limit, weight);
  if (matchEmpty(ftsQuery(query))) return [];
  let hits = run(ftsQuery(query));
  if (hits.length === 0) hits = run(ftsQueryOr(query));
  return hits;
}

function matchEmpty(match: string): boolean {
  return match.trim() === "";
}

function searchWikiMatch(
  db: DatabaseSync,
  query: string,
  match: string,
  limit: number,
  weight: number,
): Hit[] {
  const terms = match.replaceAll(" OR ", " AND ").split(" AND ");
  const words = terms.map((term) => term.replaceAll('"', "").toLowerCase());
  const seen = new Map<number, number>();
  const candidates: WikiCandidate[] = [];
  const add = (candidate: WikiCandidate): void => {
    const index = seen.get(candidate.id);
    if (index !== undefined) {
      const existing = candidates[index];
      if (existing !== undefined && candidate.rank < existing.rank) existing.rank = candidate.rank;
      return;
    }
    seen.set(candidate.id, candidates.length);
    candidates.push(candidate);
  };

  const rows = db
    .prepare(
      `SELECT p.id AS id, p.title AS title, rank FROM pages_fts
       JOIN pages p ON p.id = pages_fts.rowid
       WHERE pages_fts MATCH ? ORDER BY rank LIMIT ?`,
    )
    .all(match, limit * 4 + 10) as Array<{ id: number | bigint; title: string; rank: number }>;
  for (const row of rows) add({ id: Number(row.id), title: row.title, rank: row.rank });

  // The case-insensitive title index catches exact titles that bm25 pushes
  // below long list articles.
  for (const title of [query, query.trim().replace(/^the\s+/i, "")]) {
    const titleRows = db
      .prepare(`SELECT id, title FROM pages WHERE title = ? COLLATE NOCASE LIMIT 2`)
      .all(title) as Array<{ id: number | bigint; title: string }>;
    for (const row of titleRows) add({ id: Number(row.id), title: row.title, rank: -20 });
  }

  const titleMatch = terms.map((term) => `title:${term}`).join(" AND ");
  const titleRows = db
    .prepare(
      `SELECT p.id AS id, p.title AS title, rank FROM pages_fts
       JOIN pages p ON p.id = pages_fts.rowid
       WHERE pages_fts MATCH ? ORDER BY rank LIMIT 8`,
    )
    .all(titleMatch) as Array<{ id: number | bigint; title: string; rank: number }>;
  for (const row of titleRows) add({ id: Number(row.id), title: row.title, rank: row.rank - 6 });

  candidates.sort((a, b) => a.rank - b.rank);
  const hits: Hit[] = [];
  for (const candidate of candidates.slice(0, limit)) {
    const body = db.prepare(`SELECT text FROM pages WHERE id = ?`).get(candidate.id) as
      | { text: string }
      | undefined;
    if (body === undefined) continue;
    hits.push({
      source: "simple",
      title: candidate.title,
      ref: candidate.title,
      snippet: previewText(body.text, words),
      rank: candidate.rank * weight,
    });
  }
  return hits;
}

function searchGutenberg(db: DatabaseSync, query: string, limit: number, weight: number): Hit[] {
  const run = (match: string): Hit[] => {
    if (match.trim() === "") return [];
    const rows = db
      .prepare(
        `SELECT b.id AS id, b.title AS title, b.author AS author,
                snippet(books_fts, 0, '', '', '…', 16) AS snip,
                bm25(books_fts, 8.0, 2.0) AS rank
         FROM books_fts JOIN books b ON b.id = books_fts.rowid
         WHERE books_fts MATCH ? ORDER BY bm25(books_fts, 8.0, 2.0) LIMIT ?`,
      )
      .all(match, limit) as Array<{
      id: number | bigint;
      title: string;
      author: string;
      snip: string;
      rank: number;
    }>;
    return rows.map((row) => ({
      source: "gutenberg" as const,
      title: row.title,
      ref: String(row.id),
      snippet: `${row.snip}${row.author ? ` — ${row.author}` : ""}`.split(/\s+/).filter(Boolean).join(" "),
      rank: row.rank * weight,
    }));
  };
  let hits = run(ftsQuery(query));
  if (hits.length === 0) hits = run(ftsQueryOr(query));
  return hits.sort((a, b) => a.rank - b.rank);
}

export class Library {
  readonly dir: string;
  readonly #dbs = new Map<SourceName, DatabaseSync>();
  #closed = false;

  constructor(dir: string) {
    this.dir = dir;
  }

  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    for (const db of this.#dbs.values()) db.close();
    this.#dbs.clear();
  }

  path(source: SourceName): string {
    return join(this.dir, SOURCES[source].db);
  }

  installed(source: SourceName): boolean {
    return existsSync(this.path(source));
  }

  #open(source: SourceName): DatabaseSync {
    const cached = this.#dbs.get(source);
    if (cached !== undefined) return cached;
    const db = new DatabaseSync(this.path(source), { readOnly: true, timeout: 5000 });
    db.exec("PRAGMA busy_timeout = 5000");
    this.#dbs.set(source, db);
    return db;
  }

  search(query: string, options: { source?: SourceChoice; limit?: number } = {}): SearchResult {
    const source = options.source ?? "auto";
    const limit = Math.max(1, Math.min(30, Math.trunc(options.limit ?? 6)));
    const names: readonly SourceName[] = source === "auto" ? SOURCE_ORDER : [source];
    const hits: Hit[] = [];
    const available: SourceName[] = [];
    for (const name of names) {
      if (!this.installed(name)) continue;
      available.push(name);
      const db = this.#open(name);
      const list =
        name === "gutenberg"
          ? searchGutenberg(db, query, limit, SOURCES[name].weight)
          : searchWiki(db, query, limit, SOURCES[name].weight);
      for (const hit of list) hits.push({ ...hit, source: name });
    }
    hits.sort((a, b) => a.rank - b.rank);
    if (source === "auto" && hits.length > SOURCE_ORDER.length && limit >= SOURCE_ORDER.length) {
      // Keep one strong hit per source so a book request still sees a
      // Gutenberg title even when a short article ranks higher.
      const best = new Map<SourceName, Hit>();
      for (const hit of hits) if (!best.has(hit.source)) best.set(hit.source, hit);
      const merged: Hit[] = [];
      const seen = new Set<string>();
      for (const name of SOURCE_ORDER) {
        const hit = best.get(name);
        if (hit === undefined) continue;
        merged.push(hit);
        seen.add(`${hit.source}\0${hit.ref}`);
      }
      for (const hit of hits) {
        if (merged.length >= limit) break;
        const key = `${hit.source}\0${hit.ref}`;
        if (!seen.has(key)) {
          merged.push(hit);
          seen.add(key);
        }
      }
      return { hits: merged.slice(0, limit), available };
    }
    return { hits: hits.slice(0, limit), available };
  }

  read(source: SourceChoice, ref: string, section = 0): ReadResult {
    if (source === "auto") {
      for (const name of SOURCE_ORDER) {
        if (!this.installed(name)) continue;
        try {
          return this.#readOne(name, ref, section);
        } catch (error) {
          if (!(error instanceof LibraryNotFoundError)) throw error;
        }
      }
      throw new LibraryNotFoundError();
    }
    return this.#readOne(source, ref, section);
  }

  #readOne(source: SourceName, ref: string, section: number): ReadResult {
    if (!this.installed(source)) throw new LibraryNotFoundError();
    const db = this.#open(source);
    const loaded =
      source === "gutenberg" ? this.#gutenbergText(db, ref) : this.#wikiText(db, source, ref);
    return boundedText(source, loaded.title, loaded.ref, loaded.text, section);
  }

  #wikiText(db: DatabaseSync, source: SourceName, ref: string): { title: string; ref: string; text: string } {
    let row = db.prepare(`SELECT title, text FROM pages WHERE title = ? COLLATE NOCASE LIMIT 1`).get(ref) as
      | { title: string; text: string }
      | undefined;
    if (row === undefined) {
      const hits = searchWiki(db, ref, 1, SOURCES[source].weight);
      const hit = hits[0];
      if (hit === undefined) throw new LibraryNotFoundError();
      row = db.prepare(`SELECT title, text FROM pages WHERE title = ? COLLATE NOCASE LIMIT 1`).get(hit.ref) as
        | { title: string; text: string }
        | undefined;
    }
    if (row === undefined) throw new LibraryNotFoundError();
    return { title: row.title, ref: row.title, text: row.text };
  }

  #gutenbergText(
    db: DatabaseSync,
    ref: string,
  ): { title: string; ref: string; text: string; author: string } {
    let row = db.prepare(`SELECT id, title, author, path FROM books WHERE id = ?`).get(Number(ref)) as
      | { id: number | bigint; title: string; author: string; path: string }
      | undefined;
    if (row === undefined) {
      const match = ftsQuery(ref);
      if (match.trim() === "") throw new LibraryNotFoundError();
      row = db
        .prepare(
          `SELECT b.id AS id, b.title AS title, b.author AS author, b.path AS path
           FROM books_fts JOIN books b ON b.id = books_fts.rowid
           WHERE books_fts MATCH ? ORDER BY bm25(books_fts) LIMIT 1`,
        )
        .get(match) as { id: number | bigint; title: string; author: string; path: string } | undefined;
    }
    if (row === undefined) throw new LibraryNotFoundError();
    const info = statSync(row.path, { throwIfNoEntry: false });
    if (info === undefined || info.size > 16 << 20) {
      throw new Error("book text is unavailable or too large");
    }
    const raw = readFileSync(row.path, "utf8");
    const attribution = row.author === "" ? `Project Gutenberg #${row.id}` : `${row.author}`;
    return { title: row.title, ref: String(row.id), text: gutenbergBody(raw), author: attribution };
  }

  /** Full text used by the Twee importer; capped before it reaches a story. */
  sourceText(source: SourceChoice, ref: string): { title: string; author: string; text: string; used: SourceName } {
    const names: readonly SourceName[] = source === "auto" ? SOURCE_ORDER : [source];
    for (const name of names) {
      if (!this.installed(name)) continue;
      try {
        const read = this.read(name, ref, 0);
        const full = this.#fullText(name, ref);
        return {
          title: read.title,
          author: name === "gutenberg" ? this.#gutenbergAuthor(name, ref) : authorFor(name),
          text: full.slice(0, IMPORT_TEXT_MAX),
          used: name,
        };
      } catch (error) {
        if (!(error instanceof LibraryNotFoundError)) throw error;
      }
    }
    throw new LibraryNotFoundError();
  }

  #fullText(source: SourceName, ref: string): string {
    if (source === "gutenberg") {
      return this.#gutenbergText(this.#open(source), ref).text;
    }
    return this.#wikiText(this.#open(source), source, ref).text;
  }

  #gutenbergAuthor(_source: SourceName, ref: string): string {
    const row = this.#open("gutenberg").prepare(`SELECT author FROM books WHERE id = ?`).get(Number(ref)) as
      | { author: string }
      | undefined;
    return row?.author ?? "";
  }

  list(source: SourceChoice, limit = 20): ListResult | Record<SourceName, ListResult> {
    const capped = Math.max(1, Math.min(200, Math.trunc(limit)));
    if (source === "auto") {
      return {
        simple: this.#listOne("simple", capped),
        enwiki: this.#listOne("enwiki", capped),
        gutenberg: this.#listOne("gutenberg", capped),
      };
    }
    return this.#listOne(source, capped);
  }

  #listOne(source: SourceName, limit: number): ListResult {
    if (!this.installed(source)) return { source, available: false, total: 0, items: [] };
    const db = this.#open(source);
    if (source === "gutenberg") {
      const total = Number((db.prepare(`SELECT count(*) AS n FROM books`).get() as { n: number | bigint }).n);
      const rows = db.prepare(`SELECT id, title, author FROM books ORDER BY title LIMIT ?`).all(limit) as Array<{
        id: number | bigint;
        title: string;
        author: string;
      }>;
      return {
        source,
        available: true,
        total,
        items: rows.map((row) => ({ ref: String(row.id), title: row.title, author: row.author })),
      };
    }
    const total = Number((db.prepare(`SELECT count(*) AS n FROM pages`).get() as { n: number | bigint }).n);
    const rows = db.prepare(`SELECT title FROM pages ORDER BY title LIMIT ?`).all(limit) as Array<{ title: string }>;
    return {
      source,
      available: true,
      total,
      items: rows.map((row) => ({ ref: row.title, title: row.title })),
    };
  }
}

function authorFor(source: SourceName): string {
  return source === "simple"
    ? "Simple English Wikipedia (CC BY-SA 4.0)"
    : "Wikipedia (CC BY-SA 4.0)";
}

export function boundedText(
  source: SourceName,
  title: string,
  ref: string,
  text: string,
  section: number,
): ReadResult {
  const runes = Array.from(text);
  const total = runes.length;
  const sections = Math.max(1, Math.ceil(total / READ_CHUNK));
  const index = Math.max(0, Math.trunc(section));
  if (index >= sections) {
    return { source, title, ref, section: index, sections, text: "", chars: 0, totalChars: total };
  }
  const start = index * READ_CHUNK;
  let end = Math.min(total, start + READ_CHUNK);
  if (end - start > READ_MAX) end = start + READ_MAX;
  const result: ReadResult = {
    source,
    title,
    ref,
    section: index,
    sections,
    text: runes.slice(start, end).join(""),
    chars: end - start,
    totalChars: total,
  };
  if (index + 1 < sections) result.nextSection = index + 1;
  return result;
}

export function gutenbergBody(raw: string): string {
  const startMatch = /\*\*\*\s*START OF (?:THE|THIS) PROJECT GUTENBERG EBOOK[^\n]*\*\*\*/i.exec(raw);
  const endMatch = /\*\*\*\s*END OF (?:THE|THIS) PROJECT GUTENBERG EBOOK[^\n]*\*\*\*/i.exec(raw);
  const start = startMatch === null ? 0 : startMatch.index + startMatch[0].length;
  const end = endMatch === null ? raw.length : endMatch.index;
  return capText(raw.slice(start, end).trim());
}

function capText(text: string): string {
  return text.length > MAX_TEXT ? text.slice(0, MAX_TEXT) : text;
}

// ---- disk planner -------------------------------------------------------

export const LIBRARY_RESERVE_PERCENT = 20;
export const LIBRARY_SAFETY_PERCENT = 90;
const GIB = 1 << 30;

export interface PackSpec {
  name: SourceName;
  label: string;
  plannedBytes: number;
  dumpBytes: number;
  buildBytes: number;
}

/** Measured 2026-11-23; keep in sync with lark/internal/library/plan.go. */
export const LIBRARY_PACKS: readonly PackSpec[] = [
  { name: "simple", label: "Simple English Wikipedia", plannedBytes: GIB, dumpBytes: 360149050, buildBytes: 563585024 },
  { name: "gutenberg", label: "Project Gutenberg", plannedBytes: (12 * GIB) / 10, dumpBytes: 0, buildBytes: 2 << 20 },
  { name: "enwiki", label: "English Wikipedia", plannedBytes: 62 * GIB, dumpBytes: 25899045155, buildBytes: 34794643456 },
];

export type PackState = "installed" | "building" | "pending" | "skipped";

export interface PackPlan {
  name: SourceName;
  label: string;
  state: PackState;
  plannedBytes: number;
  installedBytes: number;
  neededBytes: number;
  percent?: number;
  reason?: string;
}

export interface LibraryPlan {
  dir: string;
  totalBytes: number;
  freeBytes: number;
  budgetBytes: number;
  reserveBytes: number;
  packs: PackPlan[];
}

export interface VolumeInfo {
  total: number;
  free: number;
}

export type StatFs = (path: string) => VolumeInfo;

export function libraryVolume(dir: string): VolumeInfo {
  let path = dir;
  for (;;) {
    try {
      const stat = statfsSync(path);
      return { total: stat.bsize * stat.blocks, free: stat.bsize * stat.bavail };
    } catch {
      const parent = dirname(path);
      if (parent === path) throw new Error(`library volume unavailable for ${dir}`);
      path = parent;
    }
  }
}

export function libraryBudget(total: number, free: number): number {
  const spendable = free - (total * LIBRARY_RESERVE_PERCENT) / 100;
  if (spendable <= 0) return 0;
  return Math.floor((spendable * LIBRARY_SAFETY_PERCENT) / 100);
}

export function humanBytes(bytes: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = Math.max(0, bytes);
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value >= 10 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

function fileSize(path: string): number {
  const stat = statSync(path, { throwIfNoEntry: false });
  return stat?.isFile() === true ? stat.size : 0;
}

function dirSize(path: string): number {
  let total = 0;
  try {
    for (const entry of readdirSync(path, { withFileTypes: true })) {
      const full = join(path, entry.name);
      total += entry.isDirectory() ? dirSize(full) : fileSize(full);
    }
  } catch {
    return 0;
  }
  return total;
}

function packDB(dir: string, pack: PackSpec): string {
  return join(dir, SOURCES[pack.name].db);
}

function packDump(dir: string, pack: PackSpec): string {
  if (pack.dumpBytes === 0) return "";
  return join(dir, "raw", `${pack.name === "simple" ? "simplewiki" : "enwiki"}-latest-pages-articles.xml.bz2`);
}

export function packHealth(dir: string, pack: PackSpec): { ok: boolean; reason: string } {
  const path = packDB(dir, pack);
  const info = statSync(path, { throwIfNoEntry: false });
  if (info === undefined) return { ok: false, reason: "" };
  if (!info.isFile() || info.size === 0) return { ok: false, reason: "index file is empty" };
  let db: DatabaseSync;
  try {
    db = new DatabaseSync(path, { readOnly: true, timeout: 2000 });
  } catch (error) {
    return { ok: false, reason: `index cannot be opened: ${(error as Error).message}` };
  }
  try {
    const table = pack.name === "gutenberg" ? "books" : "pages";
    const row = db.prepare(`SELECT 1 AS found FROM ${table} LIMIT 1`).get();
    if (row === undefined) return { ok: false, reason: `index is missing rows in ${table}` };
    if (pack.name === "gutenberg") {
      const books = readdirSync(join(dir, "books"), { withFileTypes: true }).filter(
        (entry) => entry.isFile() && entry.name.endsWith(".txt"),
      );
      if (books.length === 0) return { ok: false, reason: "no book files" };
    }
    return { ok: true, reason: "" };
  } catch (error) {
    return { ok: false, reason: `index is unreadable: ${(error as Error).message}` };
  } finally {
    db.close();
  }
}

function installedBytes(dir: string, pack: PackSpec): number {
  let total = fileSize(packDB(dir, pack));
  if (pack.name === "gutenberg") total += dirSize(join(dir, "books"));
  return total;
}

function existingBytes(dir: string, pack: PackSpec): number {
  if (pack.name === "gutenberg") {
    return (
      fileSize(join(dir, "raw", "gutenberg.jsonl")) +
      fileSize(join(dir, "raw", "pg_catalog.csv.gz")) +
      fileSize(`${packDB(dir, pack)}.building`) +
      dirSize(join(dir, "books"))
    );
  }
  const dump = packDump(dir, pack);
  let total = fileSize(`${packDB(dir, pack)}.building`);
  if (dump !== "") total += fileSize(dump) + fileSize(`${dump}.part`);
  return total;
}

function packBuilding(dir: string, pack: PackSpec): boolean {
  return existsSync(`${packDB(dir, pack)}.build.lock`) || existsSync(`${packDB(dir, pack)}.building`);
}

export function planLibrary(dir: string, statfs: StatFs = libraryVolume): LibraryPlan {
  const { total, free } = statfs(dir);
  const plan: LibraryPlan = {
    dir,
    totalBytes: total,
    freeBytes: free,
    budgetBytes: libraryBudget(total, free),
    reserveBytes: Math.floor((total * LIBRARY_RESERVE_PERCENT) / 100),
    packs: [],
  };
  let budget = plan.budgetBytes;
  for (const pack of LIBRARY_PACKS) {
    const health = packHealth(dir, pack);
    const entry: PackPlan = {
      name: pack.name,
      label: pack.label,
      state: "pending",
      plannedBytes: pack.plannedBytes,
      installedBytes: 0,
      neededBytes: 0,
    };
    if (health.ok) {
      entry.state = "installed";
      entry.installedBytes = installedBytes(dir, pack);
    } else if (packBuilding(dir, pack)) {
      entry.state = "building";
      entry.reason = health.reason;
      entry.percent = Math.min(99.9, (100 * fileSize(`${packDB(dir, pack)}.building`)) / pack.buildBytes);
    } else {
      if (health.reason !== "") entry.reason = health.reason;
      const needed = Math.max(0, pack.plannedBytes - existingBytes(dir, pack));
      entry.neededBytes = needed;
      if (needed <= budget) {
        entry.state = "pending";
        budget -= needed;
      } else {
        entry.state = "skipped";
        entry.reason = `needs ${humanBytes(needed)}, budget ${humanBytes(budget)} (${LIBRARY_RESERVE_PERCENT}% reserve, ${100 - LIBRARY_SAFETY_PERCENT}% margin)`;
      }
    }
    plan.packs.push(entry);
  }
  return plan;
}
