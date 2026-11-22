(function (root) {
  "use strict";

  const PROTOCOL_VERSION = 1;
  const LISTEN_OPTIONS = [
    { seconds: 0, label: "Off" },
    { seconds: 30, label: "30s" },
    { seconds: 60, label: "1m" },
    { seconds: 120, label: "2m" },
    { seconds: 300, label: "5m" },
  ];

  function encodeCommand(op, fields) {
    const f = fields || {};
    switch (op) {
      case "sync":
        return { v: PROTOCOL_VERSION, op: "sync" };
      case "open_book":
      case "play_game":
        if (typeof f.app !== "string" || !f.app.trim()) {
          throw new Error("app is required");
        }
        return { v: PROTOCOL_VERSION, op, app: f.app.trim() };
      case "set_voice":
        if (typeof f.voice !== "string" || !f.voice.trim()) {
          throw new Error("voice is required");
        }
        return { v: PROTOCOL_VERSION, op: "set_voice", voice: f.voice.trim() };
      case "set_volume": {
        if (typeof f.terminal !== "string" || !f.terminal.trim()) {
          throw new Error("terminal is required");
        }
        const vol = Math.round(Number(f.volume));
        if (!Number.isFinite(vol) || vol < 0 || vol > 100) {
          throw new Error("volume must be 0..100");
        }
        return { v: PROTOCOL_VERSION, op: "set_volume", terminal: f.terminal.trim(), volume: vol };
      }
      case "set_listen": {
        const sec = Number(f.seconds);
        if (!LISTEN_OPTIONS.some((opt) => opt.seconds === sec)) {
          throw new Error("invalid listen_seconds");
        }
        return { v: PROTOCOL_VERSION, op: "set_listen", seconds: sec };
      }
      default:
        throw new Error("unknown op: " + op);
    }
  }

  function decodeUpdate(raw) {
    if (!raw || typeof raw !== "object" || raw.v !== PROTOCOL_VERSION || typeof raw.op !== "string") {
      return null;
    }
    if (raw.op === "state" || raw.op === "ack") {
      return raw;
    }
    return null;
  }

  function applyStateUpdate(prev, update) {
    const base = prev || { companion: {}, terminals: [], books: [], games: [], voices: [] };
    if (!update || update.op !== "state") return base;
    const prevPortrait = base.companion && base.companion.portrait ? base.companion.portrait : "";
    const nextComp = Object.assign({}, update.companion || {});
    if (!nextComp.portrait && prevPortrait) {
      nextComp.portrait = prevPortrait;
    }
    return {
      companion: nextComp,
      terminals: Array.isArray(update.terminals) ? update.terminals : [],
      books: Array.isArray(update.books) ? update.books : [],
      games: Array.isArray(update.games) ? update.games : [],
      voices: Array.isArray(update.voices) ? update.voices : [],
    };
  }

  const api = {
    PROTOCOL_VERSION,
    LISTEN_OPTIONS,
    encodeCommand,
    decodeUpdate,
    applyStateUpdate,
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  } else {
    root.LarkPanelCodec = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : this);
