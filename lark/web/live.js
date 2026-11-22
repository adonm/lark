import {audioBusy, canCaptureMic, endMic, handleCall, handleTTS, micStopping, playChime, playPCM, setVolume, startMic, stopCall, unlockAudio} from "./audio.js";
import {loadHistory, receiveChat, reloadArchivedHistory, resetTranscript} from "./chat.js";
import {applyVolume, browserTerminal, refreshDevices, renderCallTarget, renderDevices, renderTargets, renderVoice, renderVolume} from "./devices.js";
import {$, colors, companionName, isLive, nodes, say, screen, svgNS} from "./dom.js";
import {busyMode, idleWatch} from "./idle.js";
import {loadSaved, loadTimers} from "./library.js";
import {installer} from "./pwa.js";
import {send, state} from "./state.js";

let screenHits = [];
const occupiedCode = 4001; // wsCompanionOccupied
const updateInstall = installer();
const phoneLayout = matchMedia("(max-width: 620px), (max-width: 920px) and (orientation: portrait)");
const viewport = window.visualViewport;
function syncVisualHeight() {
  // iOS can resize only the visual viewport for the keyboard. Don't override user zoom.
  if (viewport.scale === 1) {
    document.documentElement.style.setProperty("--visual-height", `${viewport.height}px`);
  } else {
    document.documentElement.style.removeProperty("--visual-height");
  }
}
if (viewport) {
  viewport.addEventListener("resize", syncVisualHeight);
  syncVisualHeight();
}
const idle = idleWatch({
  isBusy: () => busyMode(state.view?.mode) || audioBusy() || state.view?.sending || state.view?.resetting,
  onIdle: () => pause("idle"),
});

function syncFaceSettings() {
  $("face-settings").open = !state.view?.web || !phoneLayout.matches;
}
phoneLayout.addEventListener("change", syncFaceSettings);

function updateStatus(text) {
  say("screen-status", text);
  say("screen-description", text);
}

function liveLabel({companion, web}) {
  const name = companionName(companion);
  return web ? `${name} · Live companion` : `Mirroring ${name}`;
}

function clearScene() {
  state.rev = 0;
  state.pointer = null;
  screenHits = [];
  nodes.replaceChildren();
  $("canvas-bg").setAttribute("fill", "#202432");
  $("screen-placeholder").removeAttribute("display");
  delete screen.dataset.dim;
  delete screen.dataset.interactive;
}

function select(c, web = false) {
  const id = c?.id || "";
  if (id === state.view?.id && !!web === !!state.view?.web) return;
  state.view?.loading?.abort();
  disconnect();
  const isWeb = !!web || !!state.app?.scope;
  state.view = {id, companion:c, web:isWeb};
  syncFaceSettings();
  const primary = (c?.terminals || []).find(t => t.online) || (c?.terminals || [])[0];
  setVolume(primary?.volume ?? 55);
  $("text").value = "";
  $("target").value = "";
  say("chat-note", "");
  say("tunnel-note", "");
  say("voice-note", "");
  say("timer-note", "");
  say("reassign-note", "");
  $("timer-items").replaceChildren();
  $("active-timers").hidden = true;
  loadSaved();
  renderCurrent();
  renderDevices();
  resetTranscript();
  if (id) {
    loadTimers();
    loadHistory();
    connect();
  } else say("link-state", "No companion selected");
}

function renderCurrent() {
  const c = state.view?.companion, ready = state.ws?.readyState === WebSocket.OPEN && state.rev > 0;
  const active = ready && state.view?.web;
  const paused = state.view?.paused;
  const resting = !!state.view?.parentMessage;
  const primary = (c?.terminals || []).find(t => t.online) || (c?.terminals || [])[0];
  const mode = primary?.mode && primary.mode !== "offline" ? primary.mode : "";
  $("workspace").dataset.view = state.view?.web ? "virtual" : "mirror";
  colors($("workspace"), c);
  updateInstall(state.view, state.app);
  say("chat-name", c ? companionName(c) : "Choose a companion");
  $("chat-mode").hidden = !c;
  say("chat-mode", !c ? "" : paused === "occupied" ? "In use" : paused ? "Paused" :
    state.view.web ? active ? (modeLabel[state.view.mode] || "Idle") : "Connecting" :
    isLive(c) ? mode || "Live" : c.dormant ? "Dormant" : "Offline");
  $("reconnect-overlay").hidden = !paused;
  say("reconnect-note", paused === "occupied" ? "This terminal is open in another tab." :
    paused === "lost" ? "The live connection was lost." : "Disconnected while idle. Your chat is still here.");
  $("screen-content").inert = !ready;
  $("screen-controls").hidden = !ready;
  renderVolume();
  renderVoice(c, state.view?.voicePending);
  $("web-controls").hidden = $("web-destination").hidden = !state.view?.web;
  const micAvailable = canCaptureMic();
  const busyCall = inCall();
  $("talk").disabled = !active || !micAvailable || micStopping() || busyCall || resting;
  $("sound").disabled = !active;
  $("mic-help").hidden = !state.view?.web || micAvailable;
  say("mic-help", window.isSecureContext ? "This browser can’t capture microphone audio." :
    "Mic needs localhost or an HTTPS opentunnel.xyz link");
  $("text").disabled = !c || resting || state.view.web && (!active || busyCall);
  $("text").placeholder = state.view?.parentMessage || (c?.configured === false ? "Say hi to meet me!" : "Message your companion…");
  $("send").disabled = $("text").disabled || !!state.view?.sending || !!state.view?.resetting;
  $("reset").hidden = !!state.app?.scope;
  $("reset").disabled = !c || state.view.web && !active || !!state.view?.resetting || !!state.view?.sending;
  $("refresh").disabled = !c || !!state.view?.loading || !!state.view?.resetting;
  renderCallTarget();
  idle.refresh();
}

function inCall() { return state.view?.mode === "call" || state.view?.mode === "calling"; }

function canStartCall() {
  return state.view?.web && state.rev > 0 && state.ws?.readyState === WebSocket.OPEN && !inCall() && !state.view.parentMessage && canCaptureMic();
}

function startPeerCall(target) {
  if (!target || !canStartCall()) return;
  unlockAudio();
  send({ev:"call",target});
}

function disconnect({keepFace = false} = {}) {
  idle.stop();
  clearTimeout(state.retry);
  const old = state.ws;
  state.ws = null;
  old?.close();
  endMic(true, false);
  stopCall();
  if (keepFace) {
    state.rev = 0;
    state.pointer = null;
    delete screen.dataset.interactive;
  } else clearScene();
  state.watched = null;
}

function pause(reason) {
  if (!state.view?.web) return;
  state.view.paused = reason;
  disconnect({keepFace:true});
  say("link-state", reason === "occupied" ? "Terminal in use" : "Not connected");
  updateStatus(reason === "idle" ? "Live screen paused." : "Live screen disconnected.");
  renderCurrent();
  renderDevices();
}

function connect(attempt = 0, ifFree = false) {
  if (!state.app || !state.view?.id) return;
  const view = state.view;
  if (view.web && document.hidden) return pause("idle");
  disconnect({keepFace:view.web});
  view.paused = "";
  view.mode = "";
  view.parentMessage = "";
  view.bridgeEndpoint = false;
  say("link-state", "Connecting…");
  updateStatus(view.web ? `Opening ${companionName(view.companion)} here…` : `Connecting to ${companionName(view.companion)}…`);
  renderCurrent();
  const url = new URL("/ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  url.searchParams.set("token", state.app.token);
  const socket = new WebSocket(url);
  state.ws = socket;
  if (view.web) idle.start(document.hidden);
  socket.binaryType = "arraybuffer";
  socket.onopen = () => {
    if (state.ws !== socket) return;
    loadTimers();
    const micAvailable = canCaptureMic();
    send(view.web ? {ev:"hello",role:"web",terminal_id:browserTerminal,companion_id:view.id,firmware:"web",
      if_free:ifFree,caps:{call:micAvailable,call_pcm:micAvailable,drag:true,bridge_endpoint:micAvailable}}
      : {ev:"hello",role:"web"});
  };
  socket.onmessage = e => {
    if (state.ws !== socket) return;
    if (typeof e.data === "string") {
      try { command(JSON.parse(e.data)); } catch { updateStatus("Invalid screen update."); }
    } else if (view.web) playPCM(e.data);
    idle.refresh();
  };
  socket.onclose = e => {
    if (state.ws !== socket) return;
    if (view.web && e.code === occupiedCode) return pause("occupied");
    if (view.web && document.hidden) return pause("lost");
    if (state.rev) attempt = 0;
    disconnect({keepFace:view.web});
    renderCurrent();
    say("link-state", "Reconnecting…");
    updateStatus("Live screen disconnected.");
    state.retry = setTimeout(() => connect(attempt + 1, view.web), Math.min(5000, 500 * 2 ** Math.min(attempt, 4)));
  };
}

$("reconnect-companion").onclick = () => {
  if (!state.view?.paused) return;
  unlockAudio();
  connect(0, true);
  $("chat-heading").focus({preventScroll:true});
};
document.addEventListener("visibilitychange", () => idle.visibility(document.hidden));
for (const event of ["pointerdown", "keydown", "wheel", "input", "click", "change"]) {
  document.addEventListener(event, e => { if (e.isTrusted) idle.activity(); }, {capture:true, passive:true});
}

function syncWatch(live) {
  if (state.view.web || state.ws?.readyState !== WebSocket.OPEN) return;
  const id = live ? state.view.id : "";
  if (state.watched !== id) {
    state.watched = id;
    clearScene();
    send({ev:"watch", companion_id:state.view.id});
    const name = companionName(state.view.companion);
    updateStatus(id ? `Opening ${name}’s live screen…` : `${name} is offline.`);
  }
}

const modeLabel = {idle:"Idle", thinking:"Thinking", listening:"Listening", speaking:"Speaking", sleepy:"Sleepy", calling:"Ringing", call:"In call"};

function updateCompanion({companion_id, name, companion, voice}) {
  const fields = {name, ...companion, voice};
  state.app.companionVersion++;
  const comp = (state.app.companions || []).find(c => c.id === companion_id);
  if (comp) Object.assign(comp, fields);
  if (state.view?.id === companion_id) {
    Object.assign(state.view.companion, fields);
    if (state.rev) updateStatus(liveLabel(state.view));
    renderCurrent();
  }
  renderDevices();
}

function command(msg) {
  if (msg.type === "saved_apps") {
    loadSaved();
    return;
  }
  if (msg.type === "timers") {
    if (msg.companion_id === state.view.id) loadTimers();
    return;
  }
  if (msg.type === "chat") {
    if (msg.companion_id === state.view.id) receiveChat(msg.entry);
    return;
  }
  switch (msg.op) {
    case "capabilities":
      if (state.view.web) state.view.bridgeEndpoint = msg.bridge_endpoint === true;
      break;
    case "companion":
      updateCompanion(msg);
      break;
    case "history_archived":
      if (msg.companion_id === state.view.id) reloadArchivedHistory();
      break;
    case "peers": {
      const list = msg.companions || [];
      if (state.app.scope) {
        renderTargets(list);
      } else {
        const peer = list.find(c => c.id === state.view.id);
        if (!state.view.web) {
          say("link-state", "LAN dashboard");
          syncWatch(isLive(peer));
        }
        refreshDevices();
      }
      break;
    }
    case "svg": {
      if (msg.companion_id && msg.companion_id !== state.view.id) return;
      const first = !state.rev;
      const svg = new DOMParser().parseFromString(msg.svg, "image/svg+xml").documentElement;
      if (svg.localName !== "svg" || svg.namespaceURI !== svgNS || svg.getAttribute("viewBox") !== "0 0 360 360") return;
      const focusedID = nodes.contains(document.activeElement) && document.activeElement.dataset.id;
      const imported = document.importNode(svg, true);
      const selector = "[data-id][data-lark-hit]";
      const controls = [...imported.querySelectorAll(selector)];
      if (imported.matches(selector)) controls.unshift(imported);
      screenHits = controls.map(control => [control.dataset.id, ...hitBounds(control)]).reverse();
      for (const control of controls) {
        control.setAttribute("tabindex", "0");
        control.setAttribute("role", "button");
        if (!control.hasAttribute("aria-label")) {
          control.setAttribute("aria-label", control.querySelector("[aria-label]")?.getAttribute("aria-label") || control.dataset.id);
        }
      }
      const mask = $("screen-mask");
      const primary = (state.view.companion?.terminals || []).find(t => t.online) || (state.view.companion?.terminals || [])[0];
      if (!state.view.web && primary?.shape === "rect") mask.removeAttribute("clip-path");
      else mask.setAttribute("clip-path", "url(#round-screen)");
      nodes.replaceChildren(imported);
      if (focusedID) (controls.find(el => el.dataset.id === focusedID) || controls[0])?.focus({preventScroll:true});
      state.rev = msg.rev;
      $("canvas-bg").setAttribute("fill", "#000000");
      $("screen-placeholder").setAttribute("display", "none");
      screen.dataset.interactive = "true";
      if (first) {
        say("link-state", state.app.scope ? "Private connection" : "LAN dashboard");
        updateStatus(state.view.parentMessage || liveLabel(state.view));
        loadHistory({replace:false});
        if (state.view.web) renderDevices();
      }
      renderCurrent();
      break;
    }
    case "power":
      screen.dataset.dim = String(msg.brightness < 20);
      break;
    case "parent_policy":
      if (msg.companion_id === state.view.id) {
        const wasResting = !!state.view.parentMessage;
        state.view.parentMessage = msg.message || "";
        if (state.view.parentMessage) {
          endMic(true, false, false, true);
          stopCall();
          updateStatus(state.view.parentMessage);
        } else if (wasResting) {
          updateStatus(`${companionName(state.view.companion)} is available.`);
        }
        renderCurrent();
      }
      break;
    case "mode":
      if (state.view.web) {
        state.view.mode = msg.mode;
        renderCurrent();
        renderDevices();
      }
      break;
    case "volume":
      if (!msg.companion_id || msg.companion_id === state.view.id) applyVolume(state.app, state.view, msg.level, msg.auto_volume);
      break;
    case "chime":
      if (state.view.web) playChime(msg.kind);
      break;
    case "rec":
      if (state.view.web) {
        if (msg.action === "start") startMic(msg.target || "", null, msg.endpoint);
        else if (msg.action === "stop") {
          endMic(!!msg.cancelled, !msg.cancelled, false, true);
        }
      }
      break;
    case "tts":
      if (state.view.web) handleTTS(msg);
      break;
    case "call":
      if (state.view.web) handleCall(msg);
      break;
    case "error":
      say("chat-note", "The bridge couldn’t complete the live action.");
      if (msg.status === 404 && state.view.web && !state.app.scope) refreshDevices();
      break;
  }
}

function xy(e) {
  const p = new DOMPoint(e.clientX,e.clientY).matrixTransform(screen.getScreenCTM().inverse());
  const int = v => Math.max(0,Math.min(359,Math.round(v)));
  return {x:int(p.x), y:int(p.y)};
}
function hitBounds(control) { return control.dataset.larkHit.split(" ").map(Number); }

screen.addEventListener("pointerdown", e => {
  if (e.button !== 0 || state.pointer != null || !state.rev) return;
  if (state.view.web && e.pointerType === "mouse") unlockAudio();
  const {x, y} = xy(e);
  const target = screenHits.find(([, x1, y1, x2, y2]) => x >= x1 && x <= x2 && y >= y1 && y <= y2)?.[0];
  state.pointer = {id:e.pointerId, target, x:e.clientX, y:e.clientY, last:-Infinity, dragged:false};
  screen.setPointerCapture(e.pointerId);
});
screen.addEventListener("pointermove", e => {
  if (state.pointer?.id !== e.pointerId) return;
  state.pointer.dragged ||= Math.hypot(e.clientX - state.pointer.x, e.clientY - state.pointer.y) >= 4;
  if (!state.pointer.dragged || e.timeStamp - state.pointer.last < 50) return;
  state.pointer.last = e.timeStamp;
  send({ev:"drag",id:state.pointer.target,...xy(e)});
});
screen.addEventListener("pointerup", e => {
  if (state.pointer?.id !== e.pointerId) return;
  const dragged = state.pointer.dragged || Math.hypot(e.clientX - state.pointer.x, e.clientY - state.pointer.y) >= 4;
  const id = state.pointer.target;
  state.pointer = null;
  if (state.view.web) unlockAudio();
  const point = xy(e);
  send({ev:"up",id,...point});
  if (!dragged) send({ev:"tap",...point});
});
for (const ev of ["pointercancel", "lostpointercapture"]) {
  screen.addEventListener(ev, e => { if (state.pointer?.id === e.pointerId) state.pointer = null; });
}
screen.addEventListener("keydown", e => {
  const control = e.target.closest("[data-id][role=button]");
  if (!control || (e.key !== " " && e.key !== "Enter")) return;
  e.preventDefault();
  if (e.repeat) return;
  if (state.view.web) unlockAudio();
  const [x1, y1, x2, y2] = hitBounds(control);
  send({ev:"tap",id:control.dataset.id,x:Math.round((x1+x2)/2),y:Math.round((y1+y2)/2)});
});

export {canStartCall, disconnect, renderCurrent, select, startPeerCall, updateCompanion};
