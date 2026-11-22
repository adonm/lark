import test from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { parseTwee, evalPassage } = require("./story.js");

const sampleTwee = `:: StoryTitle
The Whispering Woods

:: StoryData
{"ifid":"12345678-1234-4123-8123-1234567890ab","start":"Woods"}

:: Woods {"bg":"misty pine forest"}
You stand at a fork in the trail.
<<set $visited to true>><<set $coins to 2>>
[[Take the mossy path->Moss]]
[[Follow the lantern|Lantern]]
[[Wait]]

:: Moss
A tiny frog offers you a key.
<<set $has_key to true>>
[[Go to the gate->Gate]]

:: Lantern
The lantern warms your hands.
[[Go to the gate->Gate]]

:: Wait
You hear an owl hoot.
[[Follow the lantern|Lantern]]

:: Gate {"end":true}
<<if $has_key>>The gate clicks open to a sunlit garden!<<else>>The gate is locked, so you rest under the stars.<<endif>>
`;

test("parseTwee and evalPassage handle branching, flags, and conditions", () => {
  const story = parseTwee(sampleTwee);
  assert.equal(story.title, "The Whispering Woods");
  assert.equal(story.passages[story.start].name, "Woods");

  const flags = {};
  const s = evalPassage(story, story.start, flags, true);
  assert.equal(s.choices.length, 3);
  assert.equal(flags.visited, true);
  assert.equal(flags.coins, 2);

  // Lantern -> Gate (no key)
  const l = evalPassage(story, s.choices[1].target, flags, true);
  const g1 = evalPassage(story, l.choices[0].target, flags, true);
  assert.ok(g1.text.includes("locked"));
  assert.equal(g1.end, true);

  // Moss -> Gate (with key)
  const m = evalPassage(story, s.choices[0].target, flags, true);
  const g2 = evalPassage(story, m.choices[0].target, flags, true);
  assert.ok(g2.text.includes("sunlit garden"));
});

test("parseTwee rejects invalid Twee subset", () => {
  assert.throws(() => parseTwee(":: Start {\"end\":true}\nHi\n"), /StoryTitle/);
  assert.throws(() => parseTwee(":: StoryTitle\nT\n:: Start\n[[Go->Nowhere]]\n"), /line 4/);
  assert.throws(() => parseTwee(":: StoryTitle\nT\n:: Start {\"end\":true}\n<b>Hi</b>\n"), /HTML/);
  assert.throws(() => parseTwee(":: StoryTitle\nT\n:: Start {\"end\":true}\n<<print 1>>\n"), /unsupported macro/);
  assert.throws(() => parseTwee(":: StoryTitle\nT\n:: Start\nDead end.\n"), /dead end/);
});
