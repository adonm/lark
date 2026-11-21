const assert = require("node:assert/strict");

async function audioFixture(label) {
  const sources = [], events = [], tracks = [], constraints = [], nodes = [], tones = [];
  const connect = function () { return this; };
  const context = {
    state: "running",
    currentTime: 0,
    resume() { this.state = "running"; },
    audioWorklet: {addModule: async () => {}},
    createMediaStreamSource: () => ({connect, disconnect() {}}),
    createGain: () => ({
      gain:{value:0, setValueAtTime() {}, exponentialRampToValueAtTime() {}},
      connect, disconnect() {},
    }),
    createOscillator: () => ({
      frequency:{}, connect, disconnect() {},
      start(at) { this.at = at; tones.push(this); },
      stop(at) { this.end = at; },
    }),
    createBuffer: (_, count, rate) => ({duration: count / rate, getChannelData: () => new Float32Array(count)}),
    createBufferSource() {
      return {
        connect() {},
        disconnect() {},
        start(at) { this.at = at; sources.push(this); },
        stop() { this.stopped = true; },
      };
    },
  };
  const dom = new Map();
  Object.assign(globalThis, {
    AudioContext: class { constructor() { return context; } },
    AudioWorkletNode: class {
      constructor(_, __, options) { this.options = options; nodes.push(this); }
      port = {messages:[], postMessage(msg) { this.messages.push(msg); }, close() { this.closed = true; }};
      connect = connect;
      disconnect() { this.disconnected = true; }
    },
    document: {
      addEventListener() {},
      getElementById(id) {
        if (!dom.has(id)) dom.set(id, {addEventListener() {}, setAttribute() {}, showModal() {}});
        return dom.get(id);
      },
    },
    matchMedia: () => ({addEventListener() {}}),
    WebSocket: {OPEN: 1},
  });
  const mediaDevices = {
    async getUserMedia(options) {
      constraints.push(options);
      const track = {stop() { this.stopped = true; }};
      tracks.push(track);
      return {getTracks: () => [track]};
    },
  };
  Object.defineProperty(globalThis, "navigator", {configurable:true, value:{mediaDevices}});
  globalThis.window = {isSecureContext:true, AudioWorkletNode};
  const audio = await import(`../web/audio.js?${label}`);
  const {state} = await import("../web/state.js");
  state.view = {virtual:true};
  state.ws = {readyState:1, bufferedAmount:0, send:raw => events.push(typeof raw === "string" ? JSON.parse(raw) : raw)};
  assert.equal(await audio.unlockAudio(), true);
  return {audio, sources, events, tracks, constraints, nodes, tones, context, state, dom, mediaDevices};
}

module.exports = {audioFixture};
