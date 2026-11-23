import assert from "node:assert/strict"
import test from "node:test"
import plugin from "./lark-context/index.js"

const state = "\n[state: now: 2026-11-23 12:00 AEDT (+11:00) · form: a fox · listen: off]"
const user = (text, metadata = { larkSource: "voice" }) => ({ role: "user", content: [{ type: "text", text }], metadata })
const assistant = (text) => ({ role: "assistant", content: [{ type: "text", text }] })

async function setup() {
  const hooks = new Map()
  await plugin.setup({ session: { hook: (name, callback) => hooks.set(name, callback) } })
  return hooks
}

test("context keeps only the current user message's state", async () => {
  const hooks = await setup()
  const messages = [
    user("First question" + state),
    assistant("First answer."),
    user("Second question" + state),
    assistant("Working on it."),
    user("Second question" + state),
  ]
  await hooks.get("context")({ messages })
  assert.deepEqual(messages.map((m) => m.content[0].text), [
    "First question",
    "First answer.",
    "Second question",
    "Working on it.",
    "Second question" + state,
  ])
})

test("compaction drops every state line", async () => {
  const hooks = await setup()
  const messages = [user("First" + state), assistant("Answer."), user("Second" + state)]
  await hooks.get("compaction")({ messages })
  assert.deepEqual(messages.map((m) => m.content[0].text), ["First", "Answer.", "Second"])
})

test("stripping is idempotent", async () => {
  const hooks = await setup()
  const messages = [user("Question" + state), assistant("Answer."), user("Current" + state)]
  await hooks.get("context")({ messages })
  const once = messages.map((m) => m.content[0].text)
  await hooks.get("context")({ messages })
  assert.deepEqual(messages.map((m) => m.content[0].text), once)
  assert.equal(once[2], "Current" + state)
})

test("leaves messages without a state block or Lark tag untouched", async () => {
  const hooks = await setup()
  const quoted = "The model quoted [state: now: earlier] in prose."
  const messages = [
    user("Synthetic seeding", {}),
    assistant(quoted),
    { role: "tool", content: [{ type: "text", text: "tool output [state: x]" }] },
    user("Turn with a state line" + state, {}),
  ]
  await hooks.get("context")({ messages })
  assert.equal(messages[0].content[0].text, "Synthetic seeding")
  assert.equal(messages[1].content[0].text, quoted)
  assert.equal(messages[2].content[0].text, "tool output [state: x]")
  // The [state: marker is Lark-only; a user row without the metadata tag still
  // matches, but never an assistant or tool row.
  assert.equal(messages[3].content[0].text, "Turn with a state line" + state)
  await hooks.get("compaction")({ messages })
  assert.equal(messages[3].content[0].text, "Turn with a state line")
})
