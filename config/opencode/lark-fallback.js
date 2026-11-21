function modelRef(spec) {
  const match = typeof spec === "string" && /^([^/#\s]+)\/([^#\s]+)(?:#([^#\s]+))?$/.exec(spec)
  if (!match) throw new Error("Lark fallback must be provider/model or provider/model#variant")
  const [, providerID, id, variant] = match
  return variant ? { providerID, id, variant } : { providerID, id }
}

const sameModel = (a, b) =>
  a && b && a.providerID === b.providerID && a.id === b.id && a.variant === b.variant
const sources = new Set(["typed", "voice", "tap", "timer"])
const lark = (event) => event.kind === "primary" && event.agent === "lark"
const rejected = (status) => status === 429 || status === 503 || status === 403
const isTurnBoundary = (row) => ["user", "idle", "synthetic", "shell"].includes(row.type)
const transient = ({ status, type }) =>
  (status === 429 && ["provider.rate-limit", "provider.quota", "provider.model-budget-limit"].includes(type)) ||
  (status === 503 && type === "provider.internal")

function unavailable({ status, type, response }) {
  if (status !== 403 || type !== "provider.auth") return false
  // Console's Gemini route encodes this endpoint rejection as a Google error.
  // OpenCode classifies the HTTP status as auth; do not accept other 403s.
  try {
    const body = JSON.parse(response?.body).error
    return body.code === 403 && body.status === "UNKNOWN" &&
      body.message === "Upstream request failed: Endpoint is unavailable."
  } catch {
    return false
  }
}

export default {
  id: "lark.model-fallback",
  async setup({ agent, session, options }) {
    const fallback = modelRef(options?.fallback)
    const responses = new Map()
    const turns = new Map()
    const history = ({ sessionID }) => session.context({ sessionID }).catch(() => [])

    await session.hook("http.request", (event) => {
      if (lark(event)) responses.delete(event.sessionID)
    })
    await session.hook("http.response", (event) => {
      if (lark(event) && rejected(event.response.status))
        responses.set(event.sessionID, { status: event.response.status, model: event.model })
    })
    await session.hook("retry", async (event) => {
      if (event.agent !== "lark") return
      const { sessionID, model } = event
      if (turns.has(sessionID) && sameModel(model, fallback)) {
        event.decision = { retry: false }
        return
      }
      const response = responses.get(sessionID)
      if (!sameModel(model, response?.model) || response.status !== event.error.status) return
      responses.delete(sessionID)
      if (!transient(event.error) && !(model.providerID === "opencode" && unavailable(event.error))) return
      const selected = await agent.get({ agentID: "lark" }).catch(() => undefined)
      const primary = selected?.model ?? selected?.data?.model
      // OpenCode's request-level routing cannot change providers.
      if (!sameModel(model, primary) || primary.providerID !== fallback.providerID || sameModel(model, fallback)) return
      const rows = await history(event)
      const index = rows.findLastIndex(isTurnBoundary)
      const user = rows[index]
      if (user?.type !== "user" || !sources.has(user.metadata?.larkSource)) return
      event.decision = { retry: false }
      // A non-stream HTTP rejection cannot deliver a tool call. Any row after the
      // user means this may be a continuation or already have published output.
      if (event.attempt !== 2 || index !== rows.length - 1) return
      const turn = { id: user.id, primary }
      turns.set(sessionID, turn)
      event.decision = { retry: true, delay: 0 }
      // OpenCode waits for the execution to settle, including cancellation.
      void session.wait({ sessionID }).catch(() => {}).finally(() => {
        if (turns.get(sessionID) === turn) turns.delete(sessionID)
      })
    })
    await session.hook("experimental.model.resolve", async (event) => {
      const turn = turns.get(event.sessionID)
      if (!turn || !lark(event) || !sameModel(event.model, turn.primary)) return
      const rows = await history(event)
      if (rows.findLast(isTurnBoundary)?.id === turn.id) event.model = { ...fallback }
    })
  },
}
