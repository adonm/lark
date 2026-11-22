(function (root) {
  "use strict";

  const PROTOCOL_VERSION = 1;
  const LISTEN_OPTIONS = [
    { seconds: 0, label: "Off" }, { seconds: 30, label: "30s" },
    { seconds: 60, label: "1m" }, { seconds: 120, label: "2m" }, { seconds: 300, label: "5m" },
  ];
  const HISTORY_CHANNELS = [
    { id: "all", label: "All" }, { id: "in_person", label: "In person" },
    { id: "deltachat", label: "Delta Chat" }, { id: "intercom", label: "Intercom" },
  ];

  function reqStr(v, msg) {
    if (typeof v !== "string" || !v.trim()) throw new Error(msg);
    return v.trim();
  }

  function encodeCommand(op, fields) {
    const f = fields || {};
    switch (op) {
      case "sync": return { v: PROTOCOL_VERSION, op: "sync" };
      case "say": return { v: PROTOCOL_VERSION, op: "say", text: reqStr(f.text, "text is required").slice(0, 500) };
      case "open_story":
      case "restart_story": return { v: PROTOCOL_VERSION, op, app: reqStr(f.app, "story id is required") };
      case "set_voice": return { v: PROTOCOL_VERSION, op: "set_voice", voice: reqStr(f.voice, "voice is required") };
      case "set_volume": {
        const vol = Math.round(Number(f.volume));
        if (!Number.isFinite(vol) || vol < 0 || vol > 100) throw new Error("volume must be 0..100");
        return { v: PROTOCOL_VERSION, op: "set_volume", terminal: reqStr(f.terminal, "terminal is required"), volume: vol };
      }
      case "set_listen": {
        const sec = Number(f.seconds);
        if (!LISTEN_OPTIONS.some((o) => o.seconds === sec)) throw new Error("invalid listen_seconds");
        return { v: PROTOCOL_VERSION, op: "set_listen", seconds: sec };
      }
      case "history": {
        const ch = typeof f.channel === "string" && f.channel ? f.channel : "all";
        if (!HISTORY_CHANNELS.some((c) => c.id === ch)) throw new Error("invalid history channel");
        const out = { v: PROTOCOL_VERSION, op: "history", channel: ch };
        if (typeof f.q === "string" && f.q.trim()) out.q = f.q.trim().slice(0, 100);
        if (Number(f.before_ts) > 0 && typeof f.before_id === "string" && f.before_id) {
          out.before_ts = Number(f.before_ts);
          out.before_id = f.before_id;
        }
        if (Number(f.limit) > 0) out.limit = Math.min(50, Math.round(Number(f.limit)));
        return out;
      }
      default: throw new Error("unknown op: " + op);
    }
  }

  function decodeUpdate(raw) {
    if (!raw || typeof raw !== "object" || raw.v !== PROTOCOL_VERSION || typeof raw.op !== "string") return null;
    return ["state", "ack", "live_state", "history"].includes(raw.op) ? raw : null;
  }

  function applyStateUpdate(prev, update) {
    const base = prev || { companion: {}, terminals: [], stories: [], voices: [] };
    if (!update) return base;
    if (update.op === "live_state") {
      return Object.assign({}, base, { companion: Object.assign({}, base.companion || {}, { live_state: update.live_state || "idle" }) });
    }
    if (update.op !== "state") return base;
    const prevPortrait = base.companion && base.companion.portrait ? base.companion.portrait : "";
    const nextComp = Object.assign({}, update.companion || {});
    if (!nextComp.portrait && prevPortrait) nextComp.portrait = prevPortrait;
    return {
      companion: nextComp,
      terminals: Array.isArray(update.terminals) ? update.terminals : [],
      stories: Array.isArray(update.stories) ? update.stories : [],
      voices: Array.isArray(update.voices) ? update.voices : [],
    };
  }

  const api = { PROTOCOL_VERSION, LISTEN_OPTIONS, HISTORY_CHANNELS, encodeCommand, decodeUpdate, applyStateUpdate };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.LarkPanelCodec = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
