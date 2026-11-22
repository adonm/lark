const MAGIC = 0x4c;
export const PROTO_VERSION = 1;
export const HDR_SIZE = 8;
export const MAX_PAYLOAD = 320 * 1024;

export const MSG = {
  HELLO: 0x01, RESUME: 0x02, TELEMETRY: 0x03, POINTER: 0x04, WAKE_WORD: 0x05,
  REC_START: 0x06, REC_END: 0x07, TTS_ACK: 0x08, CALL_REQ: 0x09, MEDIA: 0x0a,
  TEXT_INPUT: 0x0b, PROMPT_REPLY: 0x0c, WATCH: 0x0d, SET_VOLUME: 0x0e, PING: 0x0f, PONG: 0x10,
  WELCOME: 0x81, REJECT: 0x82, FRAME: 0x83, FRAME_SVG: 0x84, WAKE_WORD_CTRL: 0x85,
  REC_CTRL: 0x86, TTS_CTRL: 0x87, TTS_PCM: 0x88, CALL_CTRL: 0x89, CHIME: 0x8a,
  POWER: 0x8b, VOLUME: 0x8c, STATUS: 0x8d, VOICE_OWNER: 0x8e, PROMPT: 0x8f,
  PROMPT_ENDED: 0x90, COMPANION: 0x91, PEERS: 0x92, EVENT: 0x93,
};

export const KIND = { HARDWARE: 0, WEB: 1 };
export const DISP = { RGB565: 1 << 0, SVG: 1 << 1 };
export const SHAPE = { ROUND: 0, RECT: 1 };
export const HELLO_FLAG = { IF_FREE: 1 << 0 };
export const CAP = {
  BATTERY: 1 << 0, HEAD_PADS: 1 << 1, CALL: 1 << 2, CALL_PCM: 1 << 3,
  DRAG: 1 << 4, POINTER_REV: 1 << 5, BRIDGE_ENDPOINT: 1 << 6, PANEL_SLEEP: 1 << 7, WAKE_WORD: 1 << 8,
};
export const REJECT_REASON = { 1: "bad_token", 2: "bad_version", 3: "unknown_terminal", 4: "full", 5: "occupied", 6: "bad_caps" };
export const STATE = ["idle", "listening", "thinking", "speaking", "blocked", "done", "error"];
export const PROMPT_KIND = ["none", "permission", "question"];
export const PTR = { tap: 0, drag: 1, up: 2 };
export const REC_OP = { 0: "stop", 1: "start", 2: "cancel" };
export const TTS_OP = { 0: "stop", 1: "start", 2: "end" };
export const CALL_OP = { 0: "end", 1: "start", 2: "mute", 3: "unmute" };
export const CHIME = { 0: "tap", 1: "think", 2: "cancel", 3: "intercom", 4: "ring" };
export const EVENT_KIND = { 1: "saved_apps", 2: "timers", 3: "chat", 4: "history_archived", 5: "parent_policy" };
export const RTP_PT = { REC_PCM: 96, CALL_UP: 97, CALL_DOWN: 98 };

const enc = new TextEncoder();
const dec = new TextDecoder();
const str = s => enc.encode(s || "");

export function packFrame(type, seq, payload) {
  const body = payload instanceof Uint8Array ? payload : new Uint8Array(payload || 0);
  const out = new Uint8Array(HDR_SIZE + body.byteLength);
  const dv = new DataView(out.buffer);
  dv.setUint8(0, MAGIC);
  dv.setUint8(1, type);
  dv.setUint16(2, seq & 0xffff, true);
  dv.setUint32(4, body.byteLength, true);
  if (body.byteLength) out.set(body, HDR_SIZE);
  return out;
}

export function unpackFrame(buf) {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.byteLength < HDR_SIZE || bytes[0] !== MAGIC) return null;
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const length = dv.getUint32(4, true);
  if (length > MAX_PAYLOAD || bytes.byteLength < HDR_SIZE + length) return null;
  return { type: bytes[1], seq: dv.getUint16(2, true), payload: bytes.subarray(HDR_SIZE, HDR_SIZE + length) };
}

function readStr(bytes, state, long = false) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const len = long ? dv.getUint16(state.o, true) : bytes[state.o];
  state.o += long ? 2 : 1;
  const s = dec.decode(bytes.subarray(state.o, state.o + len));
  state.o += len;
  return s;
}

export function encodeHello({
  protoVersion = PROTO_VERSION, kind = KIND.WEB, caps = 0, displayFormats = DISP.SVG,
  frameCodecs = 0, width = 360, height = 360, shape = SHAPE.ROUND, flags = 0, udpPort = 0,
  terminalId = "", companionId = "", board = "", firmware = "web", token = "",
} = {}) {
  const [tid, cid, bd, fw, tok] = [str(terminalId), str(companionId), str(board), str(firmware), str(token)];
  const out = new Uint8Array(21 + tid.length + cid.length + bd.length + fw.length + tok.length);
  const dv = new DataView(out.buffer);
  dv.setUint8(0, protoVersion);
  dv.setUint8(1, kind);
  dv.setUint32(2, caps >>> 0, true);
  dv.setUint8(6, displayFormats);
  dv.setUint8(7, frameCodecs);
  dv.setUint16(8, width, true);
  dv.setUint16(10, height, true);
  dv.setUint8(12, shape);
  dv.setUint8(13, flags);
  dv.setUint16(14, udpPort, true);
  dv.setUint8(16, tid.length);
  dv.setUint8(17, cid.length);
  dv.setUint8(18, bd.length);
  dv.setUint8(19, fw.length);
  dv.setUint8(20, tok.length);
  let o = 21;
  for (const part of [tid, cid, bd, fw, tok]) { out.set(part, o); o += part.length; }
  return out;
}

export function encodeResume({ protoVersion = PROTO_VERSION, resumeToken, lastSeq = 0, terminalId = "" }) {
  const tid = str(terminalId);
  const out = new Uint8Array(22 + tid.length);
  const dv = new DataView(out.buffer);
  dv.setUint8(0, protoVersion);
  out.set(resumeToken.subarray(0, 16), 1);
  dv.setUint32(17, lastSeq >>> 0, true);
  dv.setUint8(21, tid.length);
  out.set(tid, 22);
  return out;
}

export function encodePointer(kind, x, y, revLo = 0) {
  const out = new Uint8Array(8);
  const dv = new DataView(out.buffer);
  dv.setUint8(0, typeof kind === "string" ? (PTR[kind] ?? 0) : kind);
  dv.setUint16(2, x, true);
  dv.setUint16(4, y, true);
  dv.setUint16(6, revLo, true);
  return out;
}

export function encodeRecStart(session, target = "") {
  const t = str(target), out = new Uint8Array(3 + t.length), dv = new DataView(out.buffer);
  dv.setUint16(0, session, true); dv.setUint8(2, t.length); out.set(t, 3);
  return out;
}

export function encodeRecEnd(session, cancelled, target = "") {
  const t = str(target), out = new Uint8Array(4 + t.length), dv = new DataView(out.buffer);
  dv.setUint16(0, session, true); dv.setUint8(2, cancelled ? 1 : 0); dv.setUint8(3, t.length); out.set(t, 4);
  return out;
}

export function encodeTTSAck(aborted, audioId) {
  const out = new Uint8Array(9), dv = new DataView(out.buffer);
  dv.setUint8(0, aborted ? 1 : 0); dv.setBigUint64(1, BigInt(audioId || 0), true);
  return out;
}

export function encodeCallReq(action, callId = 0, target = "") {
  const t = str(target), out = new Uint8Array(6 + t.length), dv = new DataView(out.buffer);
  dv.setUint8(0, action); dv.setUint32(1, callId >>> 0, true); dv.setUint8(5, t.length); out.set(t, 6);
  return out;
}

export function encodeTextInput(text) {
  const t = str(text), out = new Uint8Array(2 + t.length);
  new DataView(out.buffer).setUint16(0, t.length, true); out.set(t, 2);
  return out;
}

export function encodePromptReply(id, choice) {
  const a = str(id), b = str(choice), out = new Uint8Array(2 + a.length + b.length);
  out[0] = a.length; out.set(a, 1); out[1 + a.length] = b.length; out.set(b, 2 + a.length);
  return out;
}

export function encodeWatch(companionId = "") {
  const c = str(companionId), out = new Uint8Array(1 + c.length);
  out[0] = c.length; out.set(c, 1);
  return out;
}

export function encodeSetVolume(auto, level) {
  return new Uint8Array([auto ? 1 : 0, Math.max(0, Math.min(100, level | 0))]);
}

export function encodePingPong(ts) {
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, ts >>> 0, true);
  return out;
}

export function encodeRTP(pt, seq, ts, ssrc, pcmBytes) {
  const pcm = pcmBytes instanceof Uint8Array ? pcmBytes : new Uint8Array(pcmBytes);
  const out = new Uint8Array(12 + pcm.byteLength), dv = new DataView(out.buffer);
  dv.setUint8(0, 0x80);
  dv.setUint8(1, pt & 0x7f);
  dv.setUint16(2, seq & 0xffff, false);
  dv.setUint32(4, ts >>> 0, false);
  dv.setUint32(8, ssrc >>> 0, false);
  out.set(pcm, 12);
  return out;
}

export function decodeRTP(pkt) {
  const bytes = pkt instanceof Uint8Array ? pkt : new Uint8Array(pkt);
  if (bytes.byteLength < 12 || bytes[0] !== 0x80) return null;
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  return {
    pt: bytes[1] & 0x7f, seq: dv.getUint16(2, false),
    ts: dv.getUint32(4, false), ssrc: dv.getUint32(8, false),
    pcm: bytes.subarray(12),
  };
}

export function decodeServerMsg(type, payload) {
  const b = payload instanceof Uint8Array ? payload : new Uint8Array(payload || 0);
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const s = { o: 0 };
  switch (type) {
    case MSG.WELCOME: {
      const protoVersion = b[0], flags = b[1], udpPort = dv.getUint16(2, true), ssrc = dv.getUint32(4, true);
      const maxMsgBytes = dv.getUint32(8, true), resumeWindowMs = dv.getUint32(12, true), seq = dv.getUint32(16, true);
      const resumeToken = b.slice(20, 36);
      const idLen = b[36], compLen = b[37];
      const terminalId = dec.decode(b.subarray(38, 38 + idLen));
      const companionId = dec.decode(b.subarray(38 + idLen, 38 + idLen + compLen));
      return { protoVersion, bridgeEndpoint: (flags & 1) !== 0, udpPort, ssrc, maxMsgBytes, resumeWindowMs, seq, resumeToken, terminalId, companionId };
    }
    case MSG.REJECT:
      return { reason: REJECT_REASON[b[0]] || "rejected", code: b[0], message: dec.decode(b.subarray(2, 2 + b[1])) };
    case MSG.FRAME_SVG: {
      const rev = dv.getUint32(0, true), idLen = b[4], cidLen = b[5], svgLen = dv.getUint32(6, true);
      let o = 10;
      const terminalId = dec.decode(b.subarray(o, o + idLen)); o += idLen;
      const companionId = dec.decode(b.subarray(o, o + cidLen)); o += cidLen;
      const svg = dec.decode(b.subarray(o, o + svgLen));
      return { rev, terminalId, companionId, svg };
    }
    case MSG.WAKE_WORD_CTRL: {
      s.o = 1;
      return { enabled: b[0] !== 0, terminalId: readStr(b, s), companionId: readStr(b, s) };
    }
    case MSG.REC_CTRL: {
      s.o = 4;
      return { action: REC_OP[b[0]] || "stop", flags: b[1], bridgeEndpoint: (b[1] & 0x80) !== 0, session: dv.getUint16(2, true), target: readStr(b, s) };
    }
    case MSG.TTS_CTRL:
      return { state: TTS_OP[b[0]] || "stop", audio: Number(dv.getBigUint64(1, true)) };
    case MSG.TTS_PCM:
      return { pcm: b };
    case MSG.CALL_CTRL: {
      s.o = 5;
      return { state: CALL_OP[b[0]] || "end", call: dv.getUint32(1, true), reason: readStr(b, s) };
    }
    case MSG.CHIME:
      return { kind: CHIME[b[0]] || "" };
    case MSG.POWER:
      return { brightness: b[0] };
    case MSG.VOLUME: {
      s.o = 3;
      return { level: b[0], persist: b[1] !== 0, auto: b[2] !== 0, terminalId: readStr(b, s), companionId: readStr(b, s) };
    }
    case MSG.STATUS: {
      const seq = dv.getUint32(0, true), state = STATE[b[4]] || "idle", progress = b[5], kind = PROMPT_KIND[b[6]] || "none";
      s.o = 7;
      return { seq, state, progress, kind, text: readStr(b, s, true) };
    }
    case MSG.VOICE_OWNER: {
      s.o = 4;
      return { seq: dv.getUint32(0, true), companionId: readStr(b, s), terminalId: readStr(b, s) };
    }
    case MSG.PROMPT: {
      const seq = dv.getUint32(0, true), kind = PROMPT_KIND[b[4]] || "none";
      s.o = 5;
      const id = readStr(b, s), text = readStr(b, s, true), count = b[s.o++];
      const options = [];
      for (let i = 0; i < count; i++) options.push(readStr(b, s));
      return { seq, kind, id, text, options };
    }
    case MSG.PROMPT_ENDED: {
      s.o = 4;
      return { seq: dv.getUint32(0, true), id: readStr(b, s) };
    }
    case MSG.COMPANION: {
      s.o = 4;
      return {
        seq: dv.getUint32(0, true), companionId: readStr(b, s), name: readStr(b, s), voice: readStr(b, s),
        face_bg: readStr(b, s), mark_color: readStr(b, s), accent_color: readStr(b, s),
      };
    }
    case MSG.PEERS: {
      const seq = dv.getUint32(0, true), scoped = b[4] !== 0, compCount = dv.getUint16(5, true);
      s.o = 7;
      const companions = [];
      for (let i = 0; i < compCount; i++) {
        companions.push({
          id: readStr(b, s), name: readStr(b, s), configured: b[s.o++] !== 0,
          face_bg: readStr(b, s), mark_color: readStr(b, s), accent_color: readStr(b, s),
          personality: readStr(b, s, true), voice: readStr(b, s), online: b[s.o++] !== 0,
          caps: { call: b[s.o++] !== 0 },
        });
      }
      const termCount = dv.getUint16(s.o, true); s.o += 2;
      const terminals = [];
      for (let i = 0; i < termCount; i++) {
        terminals.push({
          id: readStr(b, s), kind: b[s.o++] === 0 ? "hardware" : "web", companion_id: readStr(b, s),
          online: b[s.o++] !== 0, auto_volume: b[s.o++] !== 0, ip: readStr(b, s),
          port: dv.getUint16(s.o, true), last_seen: Number(dv.getBigInt64(s.o + 2, true)),
        });
        s.o += 10;
        const firmware = readStr(b, s);
        const capsMask = dv.getUint32(s.o, true); s.o += 4;
        terminals[terminals.length - 1].firmware = firmware;
        terminals[terminals.length - 1].caps = { call: (capsMask & CAP.CALL) !== 0 };
      }
      return { seq, scoped, companions, terminals };
    }
    case MSG.EVENT: {
      s.o = 5;
      return { seq: dv.getUint32(0, true), kind: EVENT_KIND[b[4]] || "", companionId: readStr(b, s), arg1: readStr(b, s), arg2: readStr(b, s, true) };
    }
    case MSG.MEDIA:
      return decodeRTP(b);
    case MSG.PING:
    case MSG.PONG:
      return { ts: dv.getUint32(0, true) };
    default:
      return null;
  }
}
