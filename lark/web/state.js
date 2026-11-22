import {packFrame} from "./proto.js";

export const state = {rev: 0, seq: 0, txSeq: 0, resumeToken: null, resumeExpiresAt: 0, resumeTerminalId: ""};
function sendProto(type, payload) {
  if (state.ws?.readyState !== WebSocket.OPEN) return false;
  state.ws.send(packFrame(type, state.txSeq++ & 0xffff, payload));
  return true;
}

export {sendProto};
