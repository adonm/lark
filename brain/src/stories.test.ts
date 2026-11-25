import assert from "node:assert/strict";
import { test } from "node:test";
import {
  MAX_PASSAGE_TEXT,
  evalPassage,
  filterStopTokens,
  flagsMap,
  flagsSlice,
  matchChoice,
  normalizeSpeechTokens,
  parseTwee,
  slugFromTitle,
  tweeFromText,
} from "./stories.ts";
import type { Choice } from "./stories.ts";

const SAMPLE = `:: StoryTitle
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

test("parses a valid story and runs both branches", () => {
  const story = parseTwee(SAMPLE);
  assert.equal(story.title, "The Whispering Woods");
  assert.equal(story.passages[story.start]?.name, "Woods");
  assert.equal(story.passages[story.start]?.bg, "misty pine forest");

  const flags = flagsSlice(story);
  const start = evalPassage(story, story.start, flags, true);
  assert.match(start.text, /fork in the trail/);
  assert.equal(start.choices.length, 3);
  assert.equal(flags[story.flagIdx.get("visited") ?? -1], true);
  assert.equal(flags[story.flagIdx.get("coins") ?? -1], 2);

  const lantern = start.choices[1];
  assert.ok(lantern);
  const lanternEval = evalPassage(story, lantern.target, flags, true);
  const lockedGate = evalPassage(story, lanternEval.choices[0]?.target ?? -1, flags, true);
  assert.equal(lockedGate.choices.length, 0);
  assert.match(lockedGate.text, /locked/);

  const moss = start.choices[0];
  assert.ok(moss);
  const mossEval = evalPassage(story, moss.target, flags, true);
  const openGate = evalPassage(story, mossEval.choices[0]?.target ?? -1, flags, true);
  assert.match(openGate.text, /sunlit garden/);
  assert.deepEqual(flagsMap(story, flags), { visited: true, coins: 2, has_key: true });
});

test("rejects malformed Twee with line-numbered errors", () => {
  const cases: Array<{ name: string; twee: string; want: string }> = [
    { name: "missing StoryTitle", twee: ':: Start {"end":true}\nHi\n', want: "StoryTitle" },
    { name: "missing target", twee: ":: StoryTitle\nT\n:: Start\nHi\n[[Go->Nowhere]]\n", want: "line 5:" },
    {
      name: "unreachable",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\nHi\n:: Secret {"end":true}\nHidden\n',
      want: "unreachable",
    },
    { name: "dead end", twee: ":: StoryTitle\nT\n:: Start\nNo links here.\n", want: "dead end" },
    {
      name: "ending with link",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\nDone\n[[Again->Start]]\n',
      want: "must not have links",
    },
    {
      name: "too many links",
      twee: ':: StoryTitle\nT\n:: Start\n[[A]] [[B]] [[C]] [[D]]\n:: A {"end":true}\nA\n:: B {"end":true}\nB\n:: C {"end":true}\nC\n:: D {"end":true}\nD\n',
      want: "max 3",
    },
    {
      name: "passage too long",
      twee: `:: StoryTitle\nT\n:: Start {"end":true}\n${"a".repeat(MAX_PASSAGE_TEXT + 1)}\n`,
      want: "600",
    },
    {
      name: "html rejected",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\nHello <b>world</b>\n',
      want: "line 4: HTML",
    },
    {
      name: "unsupported macro",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\n<<print "hi">>\n',
      want: "line 4: unsupported macro",
    },
    {
      name: "unclosed if",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\n<<if $x>>Hi\n',
      want: "line 4: unclosed <<if>>",
    },
    {
      name: "unsupported special passage",
      twee: ':: StoryTitle\nT\n:: StoryScript\nif (x) {}\n:: Start {"end":true}\nHi\n',
      want: "unsupported Twee special passage",
    },
    {
      name: "bad metadata",
      twee: ':: StoryTitle\nT\n:: Start {"bg": 3}\nHi\n',
      want: "metadata bg must be a string",
    },
    {
      name: "duplicate passage",
      twee: ':: StoryTitle\nT\n:: Start {"end":true}\nHi\n:: Start {"end":true}\nAgain\n',
      want: "duplicate passage",
    },
  ];
  for (const item of cases) {
    assert.throws(() => parseTwee(item.twee), new RegExp(item.want.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")), item.name);
  }
});

test("set values accept booleans, numbers, and reject words", () => {
  const valid = ':: StoryTitle\nN\n:: Start {"end":true}\n<<set $a to false>><<set $b to -2.5>>Done\n';
  const story = parseTwee(valid);
  const flags = flagsSlice(story);
  evalPassage(story, story.start, flags, true);
  assert.equal(flags[story.flagIdx.get("a") ?? -1], false);
  assert.equal(flags[story.flagIdx.get("b") ?? -1], -2.5);
  assert.throws(
    () => parseTwee(':: StoryTitle\nN\n:: Start {"end":true}\n<<set $a to maybe>>\n'),
    /must be true, false, or a number/,
  );
});

test("voice choices match by normalized overlap and ordinals", () => {
  const choices: Choice[] = [
    { label: "Take the mossy path", target: 1 },
    { label: "Follow the lantern", target: 2 },
    { label: "Wait here", target: 3 },
  ];
  const cases: Array<{ spoken: string; want: number; ok: boolean }> = [
    { spoken: "let's take the mossy path please", want: 0, ok: true },
    { spoken: "lantern", want: 1, ok: true },
    { spoken: "the first one", want: 0, ok: true },
    { spoken: "second choice", want: 1, ok: true },
    { spoken: "the last one", want: 2, ok: true },
    { spoken: "what is the weather like today", want: 0, ok: false },
    { spoken: "", want: 0, ok: false },
  ];
  for (const item of cases) {
    const result = matchChoice(item.spoken, choices);
    assert.equal(result.matched, item.ok, item.spoken);
    if (item.ok) assert.equal(result.index, item.want, item.spoken);
  }
  assert.deepEqual(filterStopTokens(normalizeSpeechTokens("The mossy path!")), ["mossy", "path"]);
  assert.deepEqual(matchChoice("anything", []), { index: 0, matched: false });
});

test("slug and import converter produce a valid linear story", () => {
  assert.equal(slugFromTitle("  The Whispering Woods! "), "the-whispering-woods");
  assert.equal(slugFromTitle("!!!"), "story");

  const raw = `<b>A Long Tale</b> ${"The whale swam past the harbour and the gull called. ".repeat(300)} [[not a link]] <<not a macro>> :: not a header`;
  const twee = tweeFromText("A Long Tale", raw, { maxPassages: 8 });
  const story = parseTwee(twee);
  assert.equal(story.title, "A Long Tale");
  assert.ok(story.passages.length >= 2);
  assert.ok(story.passages.length <= 8);
  const final = evalPassage(story, story.start, flagsSlice(story), true).choices;
  assert.equal(final.length, 1);
  let current = story.start;
  let steps = 0;
  while (steps < 20) {
    const result = evalPassage(story, current, flagsSlice(story), false);
    if (result.choices.length === 0) break;
    current = result.choices[0]?.target ?? -1;
    steps++;
  }
  assert.ok(steps > 0);
  assert.equal(story.passages[current]?.end, true);
  for (const passage of story.passages) {
    const text = evalPassage(story, story.byName.get(passage.name) ?? 0, flagsSlice(story), false).text;
    assert.ok(text.length <= MAX_PASSAGE_TEXT);
    assert.doesNotMatch(text, /<[A-Za-z/!]/);
  }
});

test("empty import still yields an ending", () => {
  const twee = tweeFromText("Empty", "   ");
  const story = parseTwee(twee);
  assert.equal(story.passages.length, 1);
  assert.equal(story.passages[story.start]?.end, true);
});

test("import converter caps passage count", () => {
  const twee = tweeFromText("Long", "word ".repeat(20000), { maxPassages: 3 });
  const story = parseTwee(twee);
  assert.equal(story.passages.length, 3);
});
