// Every Lark turn appends one live [state: ...] line to its user message. The
// stored history keeps them, but only the current turn's copy is live: older
// copies are stale and burn context. This hook runs on the request that goes to
// the model, not on stored rows.
const STATE = /\n?\[state:[^\]]*\]/g

const isText = (part) => part?.type === "text" && typeof part.text === "string"
const marked = (message) =>
  message?.metadata?.larkSource != null || message?.metadata?.lark?.companion != null
const stateful = (message) =>
  Array.isArray(message?.content) &&
  message.content.some((part) => isText(part) && part.text.includes("[state:"))

function stripMessage(message) {
  if (message?.role !== "user" || (!marked(message) && !stateful(message))) return
  for (const part of message.content) {
    if (isText(part)) part.text = part.text.replace(STATE, "")
  }
}

export function strip(messages, keepLast) {
  if (!Array.isArray(messages)) return
  let last = -1
  if (keepLast) {
    for (let i = messages.length - 1; i >= 0; i--) {
      if (messages[i]?.role === "user") {
        last = i
        break
      }
    }
  }
  messages.forEach((message, i) => {
    if (i !== last) stripMessage(message)
  })
}

export default {
  id: "lark.context",
  async setup({ session }) {
    // Tool continuations resend the current user message, so keep its state.
    await session.hook("context", (event) => strip(event.messages, true))
    // A compaction summary must not inherit one turn's clock, form or battery.
    await session.hook("compaction", (event) => strip(event.messages, false))
  },
}
