import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { identifySong, parseShazamResponse, songRecognizerReady, type SongRunner } from "./song_id.ts";

function capture(): { dir: string; wav: string } {
  const dir = mkdtempSync(join(tmpdir(), "lark-song-test-"));
  const wav = join(dir, "capture.wav");
  writeFileSync(wav, "RIFF0000WAVE", { mode: 0o600 });
  return { dir, wav };
}

test("matched song returns title, artist, and album; capture is deleted", async () => {
  const { dir, wav } = capture();
  const runner: SongRunner = async () => ({
    matched: true,
    title: "Get Lucky",
    artist: "Daft Punk",
    album: "Random Access Memories",
  });
  try {
    const result = await identifySong(wav, { runner });
    assert.deepEqual(result, {
      matched: true,
      title: "Get Lucky",
      artist: "Daft Punk",
      album: "Random Access Memories",
    });
    assert.equal(existsSync(wav), false, "capture WAV must be removed");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("no match is a clean result and still deletes the capture", async () => {
  const { dir, wav } = capture();
  try {
    assert.deepEqual(await identifySong(wav, { runner: async () => ({ matched: false }) }), { matched: false });
    assert.equal(existsSync(wav), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("missing recognizer error still deletes the capture", async () => {
  const { dir, wav } = capture();
  const runner: SongRunner = async () => {
    throw Object.assign(new Error("spawn songrec ENOENT"), { code: "ENOENT" });
  };
  try {
    await assert.rejects(identifySong(wav, { runner }), /ENOENT/);
    assert.equal(existsSync(wav), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("timeout bounds the call and deletes the capture", async () => {
  const { dir, wav } = capture();
  const runner: SongRunner = async () => await new Promise(() => undefined);
  try {
    await assert.rejects(identifySong(wav, { runner, timeoutMs: 30 }), /timed out/);
    assert.equal(existsSync(wav), false);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("Shazam discovery JSON maps to the compact shape", () => {
  const parsed = parseShazamResponse({
    matches: [
      {
        track: {
          title: "Get Lucky",
          subtitle: "Daft Punk, Pharrell Williams & Nile Rodgers",
          sections: [{ type: "SONG", metadata: [{ title: "Album", text: "Random Access Memories" }] }],
        },
      },
    ],
  });
  assert.deepEqual(parsed, {
    matched: true,
    title: "Get Lucky",
    artist: "Daft Punk, Pharrell Williams & Nile Rodgers",
    subtitle: "Daft Punk, Pharrell Williams & Nile Rodgers",
    album: "Random Access Memories",
  });
  assert.deepEqual(parseShazamResponse({ matches: [] }), { matched: false });
  assert.deepEqual(parseShazamResponse({}), { matched: false });
});

test("WASM recognizer is loaded", () => {
  assert.equal(songRecognizerReady(), true);
});
