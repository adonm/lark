import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { after, before, test } from "node:test";
import {
  LIBRARY_RESERVE_PERCENT,
  Library,
  boundedText,
  libraryBudget,
  planLibrary,
} from "./library.ts";

const dir = mkdtempSync(join(tmpdir(), "lark-library-test-"));
let library: Library;

function wikiSchema(db: DatabaseSync): void {
  db.exec(`CREATE TABLE pages (id INTEGER PRIMARY KEY, title TEXT NOT NULL, text TEXT NOT NULL)`);
  db.exec(`CREATE INDEX pages_title_nocase ON pages(title COLLATE NOCASE)`);
  db.exec(
    `CREATE VIRTUAL TABLE pages_fts USING fts5(title, text, content='pages', content_rowid='id', tokenize='porter unicode61')`,
  );
}

function addPage(db: DatabaseSync, id: number, title: string, text: string): void {
  db.prepare(`INSERT INTO pages (id, title, text) VALUES (?, ?, ?)`).run(id, title, text);
  db.prepare(`INSERT INTO pages_fts (rowid, title, text) VALUES (?, ?, ?)`).run(id, title, text);
}

before(() => {
  const simple = new DatabaseSync(join(dir, "simple.db"));
  wikiSchema(simple);
  const longBody = `Otters are playful river animals. ${"They swim and dive. ".repeat(400)}`;
  addPage(simple, 1, "Otters", longBody);
  addPage(simple, 2, "River Otter Facts", "An otter floats on its back while eating. Otters live in rivers.");
  addPage(simple, 3, "The Moon", "The Moon orbits the Earth and has no air.");
  simple.close();

  const enwiki = new DatabaseSync(join(dir, "enwiki.db"));
  wikiSchema(enwiki);
  addPage(enwiki, 1, "Otter", "Otters are carnivorous mammals in the subfamily Lutrinae.".repeat(8));
  enwiki.close();

  const gutenberg = new DatabaseSync(join(dir, "gutenberg.db"));
  gutenberg.exec(
    `CREATE TABLE books (id INTEGER PRIMARY KEY, title TEXT NOT NULL, author TEXT NOT NULL, path TEXT NOT NULL, sha256 TEXT NOT NULL)`,
  );
  gutenberg.exec(
    `CREATE VIRTUAL TABLE books_fts USING fts5(title, author, content='books', content_rowid='id', tokenize='porter unicode61')`,
  );
  mkdirSync(join(dir, "books"), { recursive: true });
  const bookPath = join(dir, "books", "42.txt");
  writeFileSync(
    bookPath,
    "Project Gutenberg header\n*** START OF THE PROJECT GUTENBERG EBOOK MOBY DICK ***\nCall me Ishmael. The whale swam.\n*** END OF THE PROJECT GUTENBERG EBOOK MOBY DICK ***\nFooter",
  );
  gutenberg
    .prepare(`INSERT INTO books (id, title, author, path, sha256) VALUES (?, ?, ?, ?, ?)`)
    .run(42, "Moby Dick", "Herman Melville", bookPath, "deadbeef");
  gutenberg
    .prepare(`INSERT INTO books_fts (rowid, title, author) VALUES (?, ?, ?)`)
    .run(42, "Moby Dick", "Herman Melville");
  gutenberg.close();

  library = new Library(dir);
});

after(() => {
  library.close();
  rmSync(dir, { recursive: true, force: true });
});

test("FTS5 search prefers an exact title and returns a bounded snippet", () => {
  const result = library.search("otters", { source: "simple", limit: 3 });
  assert.deepEqual(result.available, ["simple"]);
  assert.equal(result.hits[0]?.title, "Otters");
  assert.equal(result.hits[0]?.ref, "Otters");
  assert.match(result.hits[0]?.snippet ?? "", /otter/i);
  assert.ok((result.hits[0]?.snippet.length ?? 0) <= 260);
  assert.ok(result.hits.every((hit) => hit.source === "simple"));
});

test("auto search spans installed sources and keeps one hit per source", () => {
  const result = library.search("moby", { limit: 6 });
  assert.ok(result.available.includes("simple"));
  assert.ok(result.available.includes("gutenberg"));
  const gutenberg = result.hits.find((hit) => hit.source === "gutenberg");
  assert.equal(gutenberg?.ref, "42");
});

test("reads are bounded to 3200 characters with a next section", () => {
  const first = library.read("simple", "Otters", 0);
  assert.equal(first.chars, 3200);
  assert.equal(first.text.length, 3200);
  assert.ok(first.sections >= 2);
  assert.equal(first.nextSection, 1);
  const second = library.read("simple", "Otters", 1);
  assert.equal(second.section, 1);
  assert.ok(second.chars > 0);
  assert.ok(second.chars <= 3200);
});

test("boundedText clamps past-the-end sections without throwing", () => {
  const result = boundedText("simple", "T", "T", "short", 99);
  assert.equal(result.text, "");
  assert.equal(result.sections, 1);
  assert.equal(result.nextSection, undefined);
});

test("gutenberg reads strip the license wrapper and keep attribution", () => {
  const read = library.read("gutenberg", "42", 0);
  assert.equal(read.title, "Moby Dick");
  assert.doesNotMatch(read.text, /Gutenberg header|Footer/);
  assert.match(read.text, /Call me Ishmael/);
  const loaded = library.sourceText("gutenberg", "42");
  assert.equal(loaded.used, "gutenberg");
  assert.equal(loaded.author, "Herman Melville");
});

test("libraryBudget reserves 20% of the volume and a 10% safety margin", () => {
  assert.equal(LIBRARY_RESERVE_PERCENT, 20);
  assert.equal(libraryBudget(100, 50), 27);
  assert.equal(libraryBudget(100, 10), 0);
});

test("disk planner reports every installed pack in Simple, Gutenberg, English order", () => {
  const plan = planLibrary(dir, () => ({ total: 200 * 1024 ** 3, free: 80 * 1024 ** 3 }));
  assert.deepEqual(plan.packs.map((pack) => pack.name), ["simple", "gutenberg", "enwiki"]);
  assert.equal(plan.budgetBytes, libraryBudget(200 * 1024 ** 3, 80 * 1024 ** 3));
  assert.equal(plan.reserveBytes, Math.floor((200 * 1024 ** 3) / 5));
  assert.equal(plan.packs[0]?.state, "installed");
  assert.equal(plan.packs[1]?.state, "installed");
  assert.equal(plan.packs[2]?.state, "installed");
});

test("planner marks a missing pack pending when the budget fits", () => {
  const empty = mkdtempSync(join(tmpdir(), "lark-library-plan-"));
  try {
    const plan = planLibrary(empty, () => ({ total: 20 * 1024 ** 3, free: 15 * 1024 ** 3 }));
    assert.equal(plan.packs[0]?.name, "simple");
    assert.equal(plan.packs[0]?.state, "pending");
    assert.equal(plan.packs[1]?.state, "pending");
    assert.equal(plan.packs[2]?.state, "skipped");
  } finally {
    rmSync(empty, { recursive: true, force: true });
  }
});
