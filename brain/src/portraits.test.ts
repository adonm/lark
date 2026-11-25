import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, readdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import { PortraitManager, pngSize, portraitPrompt } from "./portraits.ts";

const BASE_PNG = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
  "base64",
);

function sheet(side = 1024): Buffer {
  const bytes = Buffer.from(BASE_PNG);
  bytes.writeUInt32BE(side, 16);
  bytes.writeUInt32BE(side, 20);
  return bytes;
}

test("pngSize reads the IHDR header", () => {
  assert.deepEqual(pngSize(sheet()), { width: 1024, height: 1024 });
  assert.equal(pngSize(Buffer.from("not png")), undefined);
});

test("portraitPrompt asks for four expressions and a flat background", () => {
  const prompt = portraitPrompt({ prompt: "a round green chameleon", form: "an old chameleon", style: "pixel" });
  assert.match(prompt, /exactly four equal quadrants/);
  assert.match(prompt, /Top-left idle/);
  assert.match(prompt, /Pixel|pixel/);
  assert.match(prompt, /Character form: an old chameleon/);
  assert.match(prompt, /PNG/);
});

test("generation is single-flight, atomic, normalised, announced and cleans superseded dirs", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-portrait-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", configured: true, portraitId: "a".repeat(32) });
  mkdirSync(join(dir, "portraits", "a".repeat(32)), { recursive: true });
  writeFileSync(join(dir, "portraits", "a".repeat(32), "sheet.png"), sheet());

  const prompts: string[] = [];
  const normalizeCalls: string[] = [];
  const announcements: string[] = [];
  const refreshed: string[] = [];
  let release: (() => void) | undefined;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });
  const manager = new PortraitManager({
    db,
    dataDir: dir,
    edge: {
      normalizePortrait: async (path: string) => {
        normalizeCalls.push(path);
        writeFileSync(join(path, "pixel_idle.png"), sheet(64));
        return 12;
      },
    },
    generate: async (prompt) => {
      prompts.push(prompt);
      await gate;
      return prompt.includes("expression sheet") ? sheet() : sheet(512);
    },
    onReady: async (companionId, portraitId) => {
      refreshed.push(`${companionId}:${portraitId}`);
    },
    announce: async (_companionId, text) => {
      announcements.push(text);
    },
  });

  const first = manager.request("greg", { prompt: "a green chameleon", form: "an old chameleon", style: "pixel" });
  const second = manager.request("greg", { prompt: "ignored while running" });
  assert.equal(manager.busy("greg"), true);
  release?.();
  const [id, sameId] = await Promise.all([first, second]);
  assert.equal(id, sameId);
  assert.equal(prompts.length, 2, "one sheet and one background request");
  assert.equal(db.getCompanion("greg")?.portraitId, id);
  assert.equal(normalizeCalls.length, 1);
  assert.ok(normalizeCalls[0]?.includes(".tmp-"));
  const remaining = readdirSync(join(dir, "portraits"));
  assert.deepEqual(remaining, [id]);
  assert.equal(refreshed.length, 1);
  assert.equal(refreshed[0], `greg:${id}`);
  assert.deepEqual(announcements, ["Ta-da! My new look is ready!"]);
});

test("a non-square sheet is rejected before touching the database", async () => {
  const dir = mkdtempSync(join(tmpdir(), "lark-portrait-bad-"));
  const db = new LarkDb(join(dir, "lark.db"));
  db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  const manager = new PortraitManager({
    db,
    dataDir: dir,
    edge: { normalizePortrait: async () => 0 },
    generate: async () => sheet(512),
  });
  await assert.rejects(manager.request("greg", { prompt: "x" }), /1024×1024/);
  assert.equal(db.getCompanion("greg")?.portraitId, "");
  assert.equal(manager.available, true);
});
