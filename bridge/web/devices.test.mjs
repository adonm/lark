import assert from "node:assert/strict";
import {test} from "node:test";

test("renders the selected live device after the dashboard modules load", async t => {
  function element(tag) {
    return {
      tag, dataset:{}, style:{setProperty() {}, removeProperty() {}}, attrs:{}, children:[], listeners:{},
      addEventListener(name, handler) { this.listeners[name] = handler; },
      setPointerCapture() {},
      getScreenCTM: () => ({inverse() {}}),
      getAttribute(name) { return this.attrs[name] ?? null; },
      hasAttribute(name) { return Object.hasOwn(this.attrs, name); },
      setAttribute(name, value) { this.attrs[name] = value; },
      removeAttribute(name) { delete this.attrs[name]; },
      replaceChildren(...children) { this.children = children; },
      insertBefore(child, next) {
        child.parent = this;
        this.children.splice(next ? this.children.indexOf(next) : this.children.length, 0, child);
      },
      replaceWith(child) {
        child.parent = this.parent;
        this.parent.children.splice(this.parent.children.indexOf(this), 1, child);
      },
      querySelector(tag) { return this.children.find(child => child.tag === tag); },
      querySelectorAll(tag) { return this.children.filter(child => child.tag === tag); },
      append(...children) {
        for (const child of children) {
          child.parent = this;
          child.isConnected = true;
        }
        this.children.push(...children);
      },
      remove() {
        this.parent.children = this.parent.children.filter(child => child !== this);
        this.isConnected = false;
      },
    };
  }
  const els = new Map();
  const activation = [];
  let audio;
  Object.assign(globalThis, {
    document: {
      createElement: element,
      querySelector: () => element("meta"),
      querySelectorAll: () => [element("meta")],
      addEventListener() {},
      getElementById(id) {
        if (!els.has(id)) els.set(id, element(id));
        return els.get(id);
      },
    },
    matchMedia: () => ({matches:true, addEventListener() {}}),
    localStorage: {getItem: key => key === "lark-phone" ? "phone-1234" : null},
    location: {href:"https://lark.test/app"},
    Option: class {},
    WebSocket: {OPEN:1},
    window: {isSecureContext:true, AudioWorkletNode:class {}, addEventListener() {}},
    ResizeObserver: class { observe() {} },
    DOMPoint: class {
      constructor(x, y) { Object.assign(this, {x,y}); }
      matrixTransform() { return this; }
    },
    AudioContext: class {
      state = "suspended";
      constructor() { audio = this; }
      createGain() { return {gain:{}, connect() {}}; }
      resume() { activation.push("resume"); this.state = "running"; }
    },
  });
  Object.defineProperty(globalThis, "navigator", {configurable:true, value:{mediaDevices:{getUserMedia() {}}}});
  const {renderDevices} = await import("./devices.js");
  const {state} = await import("./state.js");
  const d = {device_id:"lark-cake", character:"Cake", ws_connected:true};
  state.app = {devices:[d]};
  state.view = {id:d.device_id, device:d};
  renderDevices();
  const row = els.get("physical").children[0].querySelector("button");
  assert.equal(row.attrs["aria-pressed"], "true");
  assert.equal(row.children[1].textContent, "Cake");
  assert.equal(row.children[2].textContent, "Live WS");
  assert.doesNotThrow(() => row.onclick());
  d.caps = {call:true};
  const sent = [];
  state.view = {id:"phone-1234", virtual:true, mode:"idle"};
  state.rev = 1;
  state.ws = {readyState:1, send:raw => {
    const event = JSON.parse(raw);
    activation.push(event.ev);
    sent.push(event);
  }};
  renderDevices();
  const call = els.get("physical").children[0].children[1];
  assert.equal(call.hidden, false);
  assert.equal(call.disabled, false);
  assert.equal(call.attrs["aria-label"], "Call Cake");
  call.onclick();
  assert.deepEqual(sent, [{ev:"call", target:d.device_id}]);
  assert.deepEqual(activation, ["resume", "call"]);

  const stage = els.get("stage").listeners;
  const gesture = {pointerType:"touch", button:0, pointerId:1, clientX:180, clientY:250};
  state.view.mode = "calling";
  audio.state = "suspended";
  stage.pointerdown(gesture);
  stage.pointerup(gesture);
  assert.deepEqual(activation, ["resume", "call", "resume", "up", "tap"]);

  await t.test("reconnect restores history without duplicating live or replayed messages", async () => {
    const {loadHistory, receiveChat} = await import("./chat.js");
    const entry = (id, second, text) => ({id, role:"companion", text, timestamp:`2026-11-21T10:30:0${second}Z`});
    const old = entry("old", 1, "Before pause"), missed = entry("missed", 2, "While paused");
    const live = entry("live", 3, "After reconnect");
    const mockFetch = (entries) => new Response(JSON.stringify({messages:entries, has_more:false}));
    const originalFetch = globalThis.fetch;
    try {
      state.app.token = "fixture";
      state.view.device = {character:"Cake"};
      globalThis.fetch = async () => mockFetch([old]);
      await loadHistory();
      const bubbles = () => els.get("messages").children.filter(node => node.dataset.entry);
      const before = bubbles()[0];
      let deliver;
      globalThis.fetch = () => new Promise(resolve => { deliver = resolve; });
      const replay = loadHistory({replace:false});
      receiveChat(live);
      deliver(mockFetch([old, missed, {...live, text:"Stale entry"}]));
      await replay;
      assert.equal(bubbles()[0], before);
      assert.deepEqual(bubbles().map(node => node.dataset.entry), ["old", "missed", "live"]);
      assert.equal(bubbles().at(-1).children.at(-1).textContent, "After reconnect");
      globalThis.fetch = async () => mockFetch([]);
      await loadHistory({replace:false});
      assert.deepEqual(bubbles(), [], "reconnect after an archive clears the old chat");
    } finally {
      globalThis.fetch = originalFetch;
    }
  });

  await t.test("New Chat confirms the archive and clears history and pagination", async () => {
    const {loadHistory} = await import("./chat.js");
    const originalFetch = globalThis.fetch;
    const requests = [];
    let accepted = false;
    globalThis.confirm = message => {
      assert.match(message, /fresh companion and chat session/i);
      assert.match(message, /archived, not deleted/i);
      return accepted;
    };
    globalThis.fetch = async (path, options) => {
      requests.push([path, options]);
      if (options.method === "POST") return Response.json({ok:true});
      return Response.json(requests.length === 1 ? {
        messages:[{id:"last", role:"companion", text:"Before reset", timestamp:"2026-11-21T10:30:00Z"}],
        has_more:true, next_cursor:{before_ts:1795257000000, before_id:"last"},
      } : {messages:[], has_more:false});
    };
    try {
      state.view = {id:d.device_id, device:d};
      await loadHistory();
      assert.equal(els.get("load-older").hidden, false);
      await els.get("reset").onclick();
      assert.equal(requests.length, 1, "cancel leaves history alone");
      accepted = true;
      await els.get("reset").onclick();
      assert.equal(requests[1][0], `/api/devices/${d.device_id}/reset`);
      assert.equal(requests[1][1].method, "POST");
      assert.deepEqual(JSON.parse(requests[1][1].body), {archive_history:true});
      assert.equal(requests[2][0], `/api/devices/${d.device_id}/messages`);
      assert.equal(els.get("load-older").hidden, true);
      assert.deepEqual(els.get("messages").children, []);
    } finally {
      globalThis.fetch = originalFetch;
      delete globalThis.confirm;
    }
  });
});
