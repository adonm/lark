import assert from "node:assert/strict"
import test from "node:test"
import plugin from "./lark-fallback.js"

const primary = { providerID: "opencode", id: "claude-haiku-5-5", variant: "none" }
const backup = { providerID: "opencode", id: "gpt-6-luna", variant: "low" }
const rateLimit = { status: 429, type: "provider.rate-limit" }
const base = { sessionID: "ses_smoke", agent: "lark", kind: "primary", model: primary }
const user = { id: "msg_user", type: "user", metadata: { larkSource: "typed" } }

async function setup(initial = [user], model = primary, fallback = "opencode/gpt-6-luna#low") {
  const hooks = new Map()
  const rows = [...initial]
  const settled = Promise.withResolvers()
  await plugin.setup({
    agent: { get: async () => ({ model }) },
    options: { fallback },
    session: {
      hook: (name, callback) => {
        hooks.set(name, callback)
      },
      context: async () => rows,
      wait: () => settled.promise,
      // The native retry must not submit or edit a prompt or change the saved model.
    },
  })
  const run = async (name, values) => {
    const event = { ...base, ...values }
    await hooks.get(name)(event)
    return event
  }
  return {
    rows,
    async resolve(values) {
      return (await run("experimental.model.resolve", values)).model
    },
    async response(status, values) {
      await run("http.request", values)
      await run("http.response", { ...values, response: { status } })
    },
    async retry(error, values) {
      return (await run("retry", { error, attempt: 2, decision: { retry: true, delay: 2000 }, ...values })).decision
    },
    async settle() {
      settled.resolve()
      await new Promise(setImmediate)
    },
  }
}

test("routes a configured primary to a fallback with a nested model ID and variant", async () => {
  const primary = { providerID: "somewhere", id: "fast", variant: "tiny" }
  const fallback = { providerID: "somewhere", id: "family/backup", variant: "small" }
  const h = await setup([user], primary, "somewhere/family/backup#small")
  await h.response(429)
  assert.deepEqual(await h.retry(rateLimit), { retry: true, delay: 2000 }) // Unselected model.
  await h.response(429, { model: primary })
  assert.deepEqual(await h.retry(rateLimit, { model: primary }), { retry: true, delay: 0 })
  assert.deepEqual(await h.resolve({ model: primary }), fallback)
  assert.deepEqual(await h.retry(rateLimit, { model: fallback }), { retry: false })
})

test("identical references and different providers do not retry as a fallback", async () => {
  for (const fallback of ["opencode/claude-haiku-5-5#none", "external/backup"]) {
    const h = await setup([user], primary, fallback)
    await h.response(429)
    assert.deepEqual(await h.retry(rateLimit), { retry: true, delay: 2000 })
    assert.deepEqual(await h.resolve(), primary)
  }
})

test("healthy Luna stays primary without touching the prompt", async () => {
  const h = await setup()
  assert.deepEqual(await h.resolve(), primary)
  await h.response(200)
  assert.deepEqual(await h.resolve(), primary)
  assert.deepEqual(h.rows, [user])
})

test("Luna HTTP 429 retries the same user message on Haiku immediately", async () => {
  const h = await setup()
  await h.response(429)
  assert.deepEqual(await h.retry(rateLimit), { retry: true, delay: 0 })
  assert.deepEqual(await h.resolve({ kind: "title" }), primary)
  assert.deepEqual(await h.resolve(), backup)
  const answer = { id: "msg_assistant", type: "assistant", content: [{ type: "tool" }] }
  h.rows.push(answer)
  assert.deepEqual(await h.resolve(), backup)
  assert.deepEqual(h.rows, [user, answer])
  h.rows.push({ ...user, id: "msg_next" })
  assert.deepEqual(await h.resolve(), primary)
})

test("Luna HTTP 503 switches immediately; a Haiku limit is terminal", async () => {
  const h = await setup()
  await h.response(503)
  assert.deepEqual(await h.retry({ type: "provider.internal", status: 503 }), { retry: true, delay: 0 })
  assert.deepEqual(await h.resolve(), backup)
  assert.deepEqual(await h.retry(rateLimit, { model: backup }), { retry: false })
  assert.deepEqual(await h.retry({ type: "provider.transport" }, { model: backup }), { retry: false })
  assert.deepEqual(h.rows, [user])
  await h.settle()
  assert.deepEqual(await h.retry(rateLimit, { model: backup }), { retry: true, delay: 2000 })
})

test("only the exact Console endpoint-unavailable 403 switches", async () => {
  for (const message of ["Forbidden", "Upstream request failed: Endpoint is unavailable."]) {
    const h = await setup()
    const error = { type: "provider.auth", status: 403,
      response: { body: JSON.stringify({ error: { code: 403, status: "UNKNOWN", message } }) } }
    await h.response(403)
    const decision = await h.retry(error, { decision: { retry: false } })
    const unavailable = message.includes("unavailable")
    assert.deepEqual(decision, unavailable ? { retry: true, delay: 0 } : { retry: false })
    assert.deepEqual(await h.resolve(), unavailable ? backup : primary)
  }
})

test("published text and possible tool effects block a Luna replay", async () => {
  for (const content of [[{ type: "text" }], [{ type: "tool" }], []]) {
    const h = await setup([user, { id: "msg_earlier", type: "assistant", content }])
    await h.response(429)
    assert.deepEqual(await h.retry(rateLimit), { retry: false })
    assert.deepEqual(await h.resolve(), primary)
  }
})

test("ambiguous errors, policy failures, auxiliary calls and unmarked input do not switch", async () => {
  for (const [status, error, values, input] of [
    [200, rateLimit],
    [429, { type: "provider.budget-limit", status: 429 }],
    [429, { type: "permission.rejected", status: 429 }],
    [429, { type: "aborted", status: 429 }],
    [429, rateLimit, { kind: "compaction" }],
    [429, rateLimit, {}, { ...user, metadata: {} }],
  ]) {
    const h = await setup([input ?? user])
    await h.response(status, values)
    assert.deepEqual(await h.retry(error), { retry: true, delay: 2000 })
    assert.deepEqual(await h.resolve(), primary)
  }
})

test("cancellation clears a selected fallback when execution settles", async () => {
  const h = await setup()
  await h.response(429)
  assert.deepEqual(await h.retry(rateLimit), { retry: true, delay: 0 })
  await h.settle() // Cancelled before OpenCode dispatches the next physical request.
  assert.deepEqual(await h.resolve(), primary)
  assert.deepEqual(h.rows, [user])
})
