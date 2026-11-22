import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const codec = require("./codec.js");

test("encodeCommand builds versioned payloads", () => {
  assert.deepEqual(codec.encodeCommand("sync"), { v: 1, op: "sync" });
  assert.deepEqual(codec.encodeCommand("open_story", { app: "lost_island" }), {
    v: 1,
    op: "open_story",
    app: "lost_island",
  });
  assert.deepEqual(codec.encodeCommand("set_voice", { voice: "bf_lily" }), {
    v: 1,
    op: "set_voice",
    voice: "bf_lily",
  });
  assert.deepEqual(codec.encodeCommand("set_volume", { terminal: "box3-b324", volume: 65.4 }), {
    v: 1,
    op: "set_volume",
    terminal: "box3-b324",
    volume: 65,
  });
  assert.deepEqual(codec.encodeCommand("set_listen", { seconds: 120 }), {
    v: 1,
    op: "set_listen",
    seconds: 120,
  });
});

test("encodeCommand rejects invalid inputs", () => {
  assert.throws(() => codec.encodeCommand("open_story", { app: "" }));
  assert.throws(() => codec.encodeCommand("set_volume", { terminal: "box3-b324", volume: 120 }));
  assert.throws(() => codec.encodeCommand("set_listen", { seconds: 45 }));
  assert.throws(() => codec.encodeCommand("bogus"));
});

test("decodeUpdate and applyStateUpdate preserve portrait across updates", () => {
  assert.equal(codec.decodeUpdate({ v: 2, op: "state" }), null);
  assert.equal(codec.decodeUpdate({ v: 1, op: "open_book" }), null);

  const s1 = codec.applyStateUpdate(null, codec.decodeUpdate({
    v: 1,
    op: "state",
    companion: { id: "greg", name: "Greg", form: "an old chameleon", portrait: "data:image/png;base64,AAA" },
    terminals: [{ id: "box3-b324", online: true, volume: 60 }],
    stories: [],
    voices: [],
  }));
  assert.equal(s1.companion.portrait, "data:image/png;base64,AAA");

  const s2 = codec.applyStateUpdate(s1, codec.decodeUpdate({
    v: 1,
    op: "state",
    companion: { id: "greg", name: "Greg", form: "an old chameleon", listen_seconds: 60 },
    terminals: [{ id: "box3-b324", online: true, volume: 70 }],
    stories: [{ name: "lost_island", title: "Lost Island" }],
    voices: [],
  }));
  assert.equal(s2.companion.portrait, "data:image/png;base64,AAA");
  assert.equal(s2.companion.listen_seconds, 60);
  assert.equal(s2.stories.length, 1);
});
