export const state = {rev: 0};
function send(event) {
  if (state.ws?.readyState !== WebSocket.OPEN) return false;
  state.ws.send(JSON.stringify(event));
  return true;
}

export {send};
