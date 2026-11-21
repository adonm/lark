import {audioBusy, canCaptureMic, endMic, handleCall, handleTTS, micStopping, playChime, playPCM, setVolume, startMic, stopCall, unlockAudio} from "./audio.js";
import {loadHistory, receiveChat, reloadArchivedHistory, resetTranscript} from "./chat.js";
import {applyVolume, refreshDevices, renderCallTarget, renderDevices, renderTargets, renderVoice, renderVolume} from "./devices.js";
import {$, colors, companionName, isLive, nodes, say, screen, svgNS} from "./dom.js";
import {busyMode, idleWatch} from "./idle.js";
import {loadSaved, loadSavedCats, loadTimers} from "./library.js";
import {clearPhotos, stagePhotos} from "./photos.js";
import {installer} from "./pwa.js";
import {send, state} from "./state.js";

let screenHits = [];
const occupiedCode = 4001; // wsCatOccupied
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
  isBusy: () => busyMode(state.view?.mode) || audioBusy() || state.view?.sending || state.view?.resetting || state.view?.sharingPhoto,
  onIdle: () => pause("idle"),
});

function syncFaceSettings() {
  $("face-settings").open = !state.view?.virtual || !phoneLayout.matches;
}
phoneLayout.addEventListener("change", syncFaceSettings);

function updateStatus(text) {
  say("screen-status", text);
  say("screen-description", text);
}

function liveLabel({device, virtual}) {
  const name = companionName(device);
  return virtual ? `${name} · Live companion` : `Mirroring ${name}`;
}

function clearScene() {
  state.rev = 0;
  state.pointer = null;
  screenHits = [];
  nodes.replaceChildren();
  clearPhotos();
  $("canvas-bg").setAttribute("fill", "#202432");
  $("screen-placeholder").removeAttribute("display");
  delete screen.dataset.dim;
  delete screen.dataset.interactive;
}

function select(d, quick = false) {
  const id = d?.device_id || "";
  if (id === state.view?.id) return;
  state.view?.loading?.abort();
  // Virtual companions identify themselves per socket; never reuse their audio connection.
  disconnect();
  state.view = {id, device:d, virtual:d?.kind === "virtual", quick};
  syncFaceSettings();
  setVolume(d?.volume ?? 55);
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
  $("saved-cat-items").replaceChildren();
  loadSavedCats();
  renderCurrent();
  renderDevices();
  resetTranscript();
  if (id) {
    loadTimers();
    if (!quick) loadHistory();
    connect();
  } else say("link-state", "No companion selected");
}

function renderCurrent() {
  const d = state.view?.device, ready = state.ws?.readyState === WebSocket.OPEN && state.rev > 0;
  const active = ready && state.view?.virtual;
  const paused = state.view?.paused;
  const resting = !!state.view?.parentMessage;
  const mode = d?.mode && d.mode !== "offline" ? d.mode : "";
  $("workspace").dataset.view = state.view?.virtual ? "virtual" : "mirror";
  colors($("workspace"), d?.cat);
  updateInstall(state.view, state.app);
  say("chat-name", companionName(d) || "Choose a companion");
  $("chat-mode").hidden = !d;
  say("chat-mode", !d ? "" : paused === "occupied" ? "In use" : paused ? "Paused" :
    state.view.virtual ? active ? (modeLabel[state.view.mode] || "Idle") : "Connecting" :
    isLive(d) ? mode || "Live WS" : "Offline");
  $("reconnect-overlay").hidden = !paused;
  say("reconnect-note", paused === "occupied" ? "This companion is open in another tab or device." :
    paused === "lost" ? "The live connection was lost." : "Disconnected while idle. Your chat is still here.");
  $("screen-content").inert = !ready;
  $("screen-controls").hidden = !ready;
  renderVolume();
  renderVoice(d, state.view?.voicePending);
  $("phone-controls").hidden = $("phone-destination").hidden = !state.view?.virtual;
  const micAvailable = canCaptureMic();
  const busyCall = inCall();
  $("talk").disabled = !active || !micAvailable || micStopping() || busyCall || resting;
  $("sound").disabled = !active;
  $("mic-help").hidden = !state.view?.virtual || micAvailable;
  say("mic-help", window.isSecureContext ? "This browser can’t capture microphone audio." :
    "Mic needs localhost or an HTTPS opentunnel.xyz link");
  $("text").disabled = !d || resting || state.view.virtual && (!active || busyCall);
  $("text").placeholder = state.view?.parentMessage || (d?.cat?.configured === false ? "Say hi to meet me!" : "Message your companion…");
  $("send").disabled = $("text").disabled || !!state.view?.sending || !!state.view?.resetting;
  $("share-photo").disabled = !d || !!state.view?.sharingPhoto || state.view?.virtual && !active;
  $("reset").hidden = !!state.app?.scope;
  $("reset").disabled = !d || state.view.virtual && !active || !!state.view?.resetting || !!state.view?.sending;
  $("refresh").disabled = !d || state.view.quick && !active || !!state.view?.loading || !!state.view?.resetting;
  renderCallTarget();
  idle.refresh();
}

function inCall() { return state.view?.mode === "call" || state.view?.mode === "calling"; }

function canStartCall() {
  return state.view?.virtual && state.rev > 0 && state.ws?.readyState === WebSocket.OPEN && !inCall() && !state.view.parentMessage && canCaptureMic();
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
  if (!state.view?.virtual) return;
  state.view.paused = reason;
  disconnect({keepFace:true});
  say("link-state", reason === "occupied" ? "Companion in use" : "Not connected");
  updateStatus(reason === "idle" ? "Live screen paused." : "Live screen disconnected.");
  renderCurrent();
  renderDevices();
}

function connect(attempt = 0, ifFree = false) {
  if (!state.app || !state.view?.id) return;
  const cat = state.view;
  if (cat.virtual && document.hidden) return pause("idle");
  disconnect({keepFace:cat.virtual});
  cat.paused = "";
  cat.mode = "";
  cat.parentMessage = "";
  cat.bridgeEndpoint = false;
  say("link-state", "Connecting…");
  updateStatus(cat.virtual ? `Joining as ${cat.device.character}…` : `Connecting to ${cat.device.character}…`);
  renderCurrent();
  const url = new URL("/ws", location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  url.searchParams.set("token", state.app.token);
  const socket = new WebSocket(url);
  state.ws = socket;
  if (cat.virtual) idle.start(document.hidden);
  socket.binaryType = "arraybuffer";
  socket.onopen = () => {
    if (state.ws !== socket) return;
    loadTimers();
    loadSavedCats();
    const micAvailable = canCaptureMic();
    send(cat.virtual ? {ev:"hello",role:"web",device_id:cat.id,character:cat.device.character,firmware:"web",
      if_free:ifFree,caps:{call:micAvailable,call_pcm:micAvailable,drag:true,bridge_endpoint:micAvailable}}
      : {ev:"hello",role:"web"});
  };
  socket.onmessage = e => {
    if (state.ws !== socket) return;
    if (typeof e.data === "string") {
      try { command(JSON.parse(e.data)); } catch { updateStatus("Invalid screen update."); }
    } else if (cat.virtual) playPCM(e.data);
    idle.refresh();
  };
  socket.onclose = e => {
    if (state.ws !== socket) return;
    if (cat.virtual && e.code === occupiedCode) return pause("occupied");
    if (cat.virtual && document.hidden) return pause("lost");
    if (state.rev) attempt = 0;
    disconnect({keepFace:cat.virtual});
    renderCurrent();
    say("link-state", "Reconnecting…");
    updateStatus("Live screen disconnected.");
    state.retry = setTimeout(() => connect(attempt + 1, cat.virtual), Math.min(5000, 500 * 2 ** Math.min(attempt, 4)));
  };
}

$("reconnect-cat").onclick = () => {
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
  if (state.view.virtual || state.ws?.readyState !== WebSocket.OPEN) return;
  const id = live ? state.view.id : "";
  if (state.watched !== id) {
    state.watched = id;
    clearScene();
    // Always subscribe by ID: the bridge may have reconnected since the peer snapshot.
    send({ev:"watch", device_id:state.view.id});
    const d = state.view.device;
    updateStatus(id ? `Opening ${d.character}’s live screen…` : `${d.character} is offline.`);
  }
}

const modeLabel = {idle:"Idle", thinking:"Thinking", listening:"Listening", speaking:"Speaking", sleepy:"Sleepy", calling:"Ringing", call:"In call"};

function updateCat({device_id, character, cat, voice}) {
  const fields = {character, cat, voice};
  state.app.deviceVersion++;
  const device = state.app.devices.find(d => d.device_id === device_id);
  if (device) Object.assign(device, fields);
  if (state.view?.id === device_id) {
    Object.assign(state.view.device, fields);
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
    if (msg.device_id === state.view.id) loadTimers();
    return;
  }
  if (msg.type === "chat") {
    if (msg.device_id === state.view.id) receiveChat(msg.entry);
    return;
  }
  switch (msg.op) {
    case "capabilities":
      if (state.view.virtual) state.view.bridgeEndpoint = msg.bridge_endpoint === true;
      break;
    case "cat":
      updateCat(msg);
      break;
    case "saved_cats":
      loadSavedCats();
      break;
    case "cat_reassigned":
      if (msg.device_id === state.view.id) loadHistory();
      break;
    case "history_archived":
      if (msg.device_id === state.view.id) reloadArchivedHistory();
      break;
    case "peers": {
      if (state.app.scope) {
        renderTargets(msg.devices);
      } else {
        const peer = msg.devices.find(d => d.device_id === state.view.id);
        if (!state.view.virtual) {
          say("link-state", "LAN dashboard");
          syncWatch(isLive(peer));
        }
        refreshDevices();
      }
      break;
    }
    case "svg": {
      if (msg.device_id !== state.view.id) return;
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
      stagePhotos(imported, state.app.token);
      const mask = $("screen-mask");
      if (state.view.device.shape === "rect") mask.removeAttribute("clip-path");
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
        if (state.view.virtual) renderDevices();
      }
      renderCurrent();
      break;
    }
    case "power":
      screen.dataset.dim = String(msg.brightness < 20);
      break;
    case "parent_policy":
      if (msg.device_id === state.view.id) {
        const wasResting = !!state.view.parentMessage;
        state.view.parentMessage = msg.message || "";
        if (state.view.parentMessage) {
          endMic(true, false, false, true);
          updateStatus(state.view.parentMessage);
        } else if (wasResting) {
          updateStatus(`${state.view.device.character} is available.`);
        }
        renderCurrent();
      }
      break;
    case "mode":
      if (state.view.virtual) {
        state.view.mode = msg.mode;
        renderCurrent();
        renderDevices();
      }
      break;
    case "volume":
      if (!msg.device_id || msg.device_id === state.view.id) applyVolume(state.app, state.view, msg.level, msg.auto_volume);
      break;
    case "chime":
      if (state.view.virtual) playChime(msg.kind);
      break;
    case "rec":
      if (state.view.virtual) {
        if (msg.action === "start") startMic(msg.target || "", null, msg.endpoint);
        else if (msg.action === "stop") {
          // The bridge already has the endpoint; don't leave hardware tracks
          // on waiting for a worklet quantum (the audio context may suspend).
          endMic(!!msg.cancelled, !msg.cancelled, false, true);
        }
      }
      break;
    case "tts":
      if (state.view.virtual) handleTTS(msg);
      break;
    case "call":
      if (state.view.virtual) handleCall(msg);
      break;
    case "error":
      say("chat-note", "The bridge couldn’t complete the live action.");
      if (msg.status === 404 && state.view.virtual && !state.app.scope) refreshDevices();
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
  if (state.view.virtual && e.pointerType === "mouse") unlockAudio();
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
  if (state.view.virtual) unlockAudio();
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
  if (state.view.virtual) unlockAudio();
  const [x1, y1, x2, y2] = hitBounds(control);
  send({ev:"tap",id:control.dataset.id,x:Math.round((x1+x2)/2),y:Math.round((y1+y2)/2)});
});

export {canStartCall, disconnect, renderCurrent, select, startPeerCall, updateCat};
