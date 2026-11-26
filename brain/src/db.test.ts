import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { after, before, test } from "node:test";
import { LarkDb, SCHEMA_TABLES, VersionConflictError } from "./db.ts";
import type { Message } from "./db.ts";

const dir = mkdtempSync(join(tmpdir(), "lark-db-test-"));
const path = join(dir, "lark.db");
let db: LarkDb;

before(() => {
  db = new LarkDb(path);
});

after(() => {
  db.close();
  rmSync(dir, { recursive: true, force: true });
});

interface Column {
  name: string;
  type: string;
  notnull: number;
  dflt_value: string | null;
  pk: number;
}

function columns(target: DatabaseSync, table: string): Column[] {
  return target.prepare(`PRAGMA table_info(${table})`).all() as unknown as Column[];
}

function normalized(column: Column): string {
  const fallback = (column.dflt_value ?? "").replace(/^["']|["']$/g, "");
  return `${column.name}:${column.type}:${column.notnull}:${column.pk}:${fallback}`;
}

test("schema exposes every table with stable column shapes", () => {
  const tables = db
    .raw()
    .prepare(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
    .all()
    .map((row) => String(row.name));
  for (const table of SCHEMA_TABLES) assert.ok(tables.includes(table), `missing ${table}`);
  assert.deepEqual(
    columns(db.raw(), "messages").map((column) => column.name),
    ["id", "companion_id", "role", "sender", "text", "source", "card_title", "ts_ms", "channel", "peer"],
  );
  assert.deepEqual(
    columns(db.raw(), "companions").map((column) => column.name),
    [
      "id",
      "name",
      "configured",
      "personality",
      "voice",
      "updated_at",
      "dc_account_id",
      "form",
      "portrait_id",
      "listening",
      "identity_version",
      "identity_synced",
    ],
  );
});

test("schema matches the live household database when it is present", () => {
  const live = join(homedir(), ".local/share/lark/lark.db");
  if (!existsSync(live)) return;
  const liveDb = new DatabaseSync(live, { readOnly: true, timeout: 5000 });
  try {
    for (const table of SCHEMA_TABLES) {
      const expected = columns(liveDb, table).map(normalized);
      const actual = columns(db.raw(), table).map(normalized);
      assert.deepEqual(actual, expected, `schema drift in ${table}`);
    }
  } finally {
    liveDb.close();
  }
});

test("companions upsert, patch, list, and delete", () => {
  const created = db.upsertCompanion({
    id: "Cake",
    name: "Cake",
    configured: true,
    personality: "a curious cat",
    voice: "af_heart",
    updatedAt: 1000,
  });
  assert.equal(created.id, "cake");
  assert.equal(created.configured, true);
  assert.equal(created.listening, false);
  assert.equal(created.personality, "a curious cat");

  const patched = db.patchCompanion("cake", { listening: true, form: "fox", updatedAt: 2000 });
  assert.equal(patched?.listening, true);
  assert.equal(patched?.form, "fox");
  assert.equal(patched?.updatedAt, 2000);
  assert.equal(db.patchCompanion("missing", { name: "x" }), undefined);

  db.upsertCompanion({ id: "biscuit", name: "Biscuit" });
  assert.deepEqual(db.listCompanions().map((c) => c.id), ["biscuit", "cake"]);

  db.deleteCompanion("biscuit");
  assert.equal(db.getCompanion("biscuit"), undefined);
});

test("terminals keep foreign keys and volume preferences", () => {
  db.upsertCompanion({ id: "cake", name: "Cake" });
  db.upsertTerminal({
    id: "kitchen",
    kind: "terminal",
    companionId: "cake",
    ip: "10.0.0.5",
    port: 8080,
    lastSeen: 1234,
    createdAt: "2026-11-26T00:00:00Z",
  });
  assert.equal(db.getTerminal("kitchen")?.companionId, "cake");
  assert.equal(db.getTerminalVolume("kitchen"), 55);
  db.setTerminalVolume("kitchen", 140);
  assert.equal(db.getTerminalVolume("kitchen"), 100);

  db.deleteCompanion("cake");
  assert.equal(db.getTerminal("kitchen")?.companionId, null);
});

test("sessions and meta round trip", () => {
  db.putSession({
    companionId: "Cake",
    sessionId: "ses_123",
    updatedAt: 42,
    contractHash: "hash",
    contextAfterMs: 999,
  });
  assert.deepEqual(db.getSession("cake"), {
    companionId: "cake",
    sessionId: "ses_123",
    updatedAt: 42,
    contractHash: "hash",
    contextAfterMs: 999,
  });
  db.setMeta("schema_version", "7");
  assert.equal(db.getMeta("schema_version"), "7");
  db.deleteMeta("schema_version");
  assert.equal(db.getMeta("schema_version"), undefined);
});

test("memory versions are optimistic and monotonic", () => {
  const first = db.putMemory({ scope: "shared", key: "bins", value: { day: "Tuesday" }, updatedAt: 1 });
  assert.equal(first.version, 1);
  const second = db.putMemory({
    scope: "shared",
    key: "bins",
    value: { day: "Wednesday" },
    expectedVersion: 1,
    updatedAt: 2,
  });
  assert.equal(second.version, 2);
  assert.deepEqual(second.value, { day: "Wednesday" });
  assert.throws(
    () => db.putMemory({ scope: "shared", key: "bins", value: {}, expectedVersion: 1 }),
    VersionConflictError,
  );
  assert.throws(
    () => db.putMemory({ scope: "shared", key: "new", value: 1, expectedVersion: 3 }),
    VersionConflictError,
  );
  assert.equal(db.listMemory("shared").length, 1);
  db.deleteMemory("shared", "bins");
  assert.equal(db.getMemory("shared", "bins"), undefined);
});

test("stories and reading progress survive delete", () => {
  db.saveStory({
    id: "woods",
    title: "The Woods",
    twee: ":: StoryTitle\nThe Woods\n",
    author: "Lark",
    updatedAt: 5,
    passages: 3,
  });
  assert.equal(db.getStory("woods")?.title, "The Woods");
  db.saveStoryProgress({
    companionId: "cake",
    storyId: "woods",
    passage: "Start",
    page: 2,
    flags: { has_key: true, coins: 2 },
    updatedAt: 6,
  });
  const progress = db.getStoryProgress("cake", "woods");
  assert.deepEqual(progress?.flags, { has_key: true, coins: 2 });
  assert.equal(progress?.page, 2);
  db.deleteStory("woods");
  assert.equal(db.getStory("woods"), undefined);
  assert.equal(db.getStoryProgress("cake", "woods"), undefined);
});

function message(id: string, tsMs: number, extra: Partial<Message> = {}): Message {
  return {
    id,
    companionId: "cake",
    role: "user",
    sender: "Kate",
    text: `message ${id}`,
    source: "voice",
    channel: "local",
    cardTitle: "",
    tsMs,
    peer: "",
    ...extra,
  };
}

test("messages page by (ts_ms, id) cursor", () => {
  for (let i = 1; i <= 6; i++) db.appendMessage(message(`m${i}`, 1000 + i));
  const first = db.listMessages({ companionId: "cake", limit: 2 });
  assert.deepEqual(first.messages.map((m) => m.id), ["m5", "m6"]);
  assert.equal(first.hasMore, true);
  assert.deepEqual(first.nextCursor, { beforeTs: 1005, beforeId: "m5" });

  const second = db.listMessages({ companionId: "cake", limit: 2, cursor: first.nextCursor });
  assert.deepEqual(second.messages.map((m) => m.id), ["m3", "m4"]);
  assert.equal(second.hasMore, true);

  const third = db.listMessages({ companionId: "cake", limit: 2, cursor: second.nextCursor });
  assert.deepEqual(third.messages.map((m) => m.id), ["m1", "m2"]);
  assert.equal(third.hasMore, false);
  assert.equal(third.nextCursor, undefined);
});

test("history can be scoped to one remote contact", () => {
  db.deleteMessages("cake");
  db.appendMessage(message("local1", 2000, { channel: "local", sender: "Kate" }));
  db.appendMessage(message("delta1", 2001, { channel: "delta", sender: "Kate" }));
  db.appendMessage(message("delta2", 2002, { channel: "delta", sender: "Robin" }));
  const scoped = db.listMessages({ companionId: "cake", channel: "delta", sender: "Kate" });
  assert.deepEqual(scoped.messages.map((m) => m.id), ["delta1"]);
  const all = db.listMessages({ companionId: "cake" });
  assert.deepEqual(all.messages.map((m) => m.id), ["local1", "delta1", "delta2"]);
});

test("remote history keeps both sides of the scoped Delta Chat chat", () => {
  db.deleteMessages("cake");
  db.appendMessage(message("room1", 3000, { channel: "in_person", source: "voice", sender: "Someone" }));
  db.appendMessage(message("room2", 3001, { channel: "in_person", source: "voice", role: "assistant", sender: "Cake" }));
  db.appendMessage(message("sam1", 3002, { channel: "deltachat", source: "deltachat", sender: "Sam", peer: "sam@example.org" }));
  db.appendMessage(
    message("sam2", 3003, {
      channel: "deltachat",
      source: "deltachat",
      role: "assistant",
      sender: "Cake",
      peer: "sam@example.org",
    }),
  );
  db.appendMessage(message("alex1", 3004, { channel: "deltachat", source: "deltachat", sender: "Alex", peer: "alex@example.org" }));
  db.appendMessage(
    message("alex2", 3005, {
      channel: "deltachat",
      source: "deltachat",
      role: "assistant",
      sender: "Cake",
      peer: "alex@example.org",
    }),
  );

  const all = db.listRemoteMessages({
    companionId: "cake",
    channel: "all",
    contact: "sam@example.org",
    contactName: "sam",
    q: "",
    limit: 20,
  });
  // Cake's reply to Alex is private too: both peer rows stay hidden.
  assert.deepEqual(all.messages.map((m) => m.id), ["room1", "room2", "sam1", "sam2"]);

  const onlyDc = db.listRemoteMessages({
    companionId: "cake",
    channel: "deltachat",
    contact: "sam@example.org",
    contactName: "sam",
    q: "",
    limit: 20,
  });
  assert.deepEqual(onlyDc.messages.map((m) => m.id), ["sam1", "sam2"]);
});

test("remote history attributes legacy Delta Chat rows when the token has no name", () => {
  db.deleteMessages("cake");
  // v1 rows predate `peer`; the token also omitted the display name, so only
  // the address can be matched. The sibling row from another contact must not
  // leak into the page.
  db.appendMessage(message("legacy-sam", 4000, { channel: "deltachat", source: "deltachat", sender: "sam@example.org" }));
  db.appendMessage(message("legacy-alex", 4001, { channel: "deltachat", source: "deltachat", sender: "alex@example.org" }));
  const page = db.listRemoteMessages({
    companionId: "cake",
    channel: "deltachat",
    contact: "sam@example.org",
    contactName: "sam@example.org",
    q: "",
    limit: 20,
  });
  assert.deepEqual(page.messages.map((m) => m.id), ["legacy-sam"]);
});

test("legacy Delta Chat replies are attributed to the turn they answer", () => {
  db.deleteMessages("cake");
  db.setContactName("sam@example.org", "Sam");
  db.setContactName("alex@example.org", "Alex");
  db.setContactName("robin-1@example.org", "Robin");
  db.setContactName("robin-2@example.org", "Robin");
  const legacy = (id: string, tsMs: number, sender: string, role: "user" | "companion") =>
    message(id, tsMs, { channel: "deltachat", source: "deltachat", sender, role: role as Message["role"] });
  db.appendMessage(legacy("bridge-sam", 5000, "Sam", "user"));
  db.appendMessage(legacy("bridge-sam-reply", 5002, "Cake", "companion"));
  db.appendMessage(legacy("bridge-alex", 5004, "Alex", "user"));
  db.appendMessage(legacy("bridge-alex-reply", 5006, "Cake", "companion"));
  // Two contacts share a display name, so neither row is safe to attribute.
  db.appendMessage(legacy("bridge-robin", 5008, "Robin", "user"));
  db.appendMessage(legacy("bridge-robin-reply", 5010, "Cake", "companion"));

  assert.equal(db.backfillLegacyPeers(), 4);
  const scoped = (contact: string) =>
    db
      .listRemoteMessages({ companionId: "cake", channel: "deltachat", contact, contactName: contact, q: "", limit: 20 })
      .messages.map((m) => m.id);
  assert.deepEqual(scoped("sam@example.org"), ["bridge-sam", "bridge-sam-reply"]);
  assert.deepEqual(scoped("alex@example.org"), ["bridge-alex", "bridge-alex-reply"]);
  assert.deepEqual(scoped("robin-1@example.org"), []);
  assert.equal(db.backfillLegacyPeers(), 0);
});

test("upgraded databases gain the peer column", () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-db-migrate-"));
  const path = join(dir, "legacy.db");
  const legacy = new DatabaseSync(path);
  legacy.exec(
    `CREATE TABLE messages (id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL, channel TEXT NOT NULL DEFAULT '')`,
  );
  legacy.prepare(`INSERT INTO messages (id, companion_id, role, sender, text, source, ts_ms) VALUES ('m1', 'cake', 'user', 'Kate', 'hi', 'voice', 1)`).run();
  legacy.close();
  const upgraded = new LarkDb(path);
  try {
    assert.ok(columns(upgraded.raw(), "messages").some((column) => column.name === "peer"));
    assert.equal(upgraded.listMessages({ companionId: "cake" }).messages[0]?.peer, "");
  } finally {
    upgraded.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
