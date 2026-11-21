import assert from "node:assert/strict";
import {test} from "node:test";
import {transcript} from "./transcript.js";

test("live and older bubbles maintain order and day headings without remounting their neighbors", () => {
  const children = [];
  const list = {
    insertBefore(node, next) { children.splice(next ? children.indexOf(next) : children.length, 0, node); },
    replaceChildren() { children.length = 0; },
  };
  function el(label) {
    return {label,
      get offsetTop() { return children.indexOf(this) * 20; },
      offsetHeight:20,
      remove() { children.splice(children.indexOf(this), 1); },
      replaceWith(next) { children.splice(children.indexOf(this), 1, next); },
    };
  }
  const chat = transcript(list, e => el(e.text), day => el(`day ${day}`), when => when.toISOString().slice(0, 10));
  const on = (id, day, text = id) => ({id, timestamp:`2026-11-${day}T00:00:00Z`, text});
  const labels = () => children.map(n => n.label);
  chat.upsert(on("b", 21));
  const bubbleB = chat.node("b");
  chat.upsert(on("z", 22));
  chat.upsert(on("a", 21)); // Same millisecond as b, received out of order.
  chat.upsert(on("old", 20));
  assert.deepEqual(labels(), ["day 2026-11-20", "old", "day 2026-11-21", "a", "b", "day 2026-11-22", "z"]);
  assert.equal(chat.node("b"), bubbleB);
  assert.equal(chat.anchor(80).entry.id, "b");
  chat.upsert(on("a", 21, "updated"));
  assert.deepEqual(labels(), ["day 2026-11-20", "old", "day 2026-11-21", "updated", "b", "day 2026-11-22", "z"]);
  chat.upsert(on("old", 22)); // Timestamp correction moves the bubble and both headings.
  assert.deepEqual(labels(), ["day 2026-11-21", "updated", "b", "day 2026-11-22", "old", "z"]);
  assert.equal(chat.node("b"), bubbleB);
});
