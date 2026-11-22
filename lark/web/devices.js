import {api} from "./api.js";
import {setVolume, unlockAudio, volume} from "./audio.js";
import {loadHistory} from "./chat.js";
import {$, colors, companionName, getLocal, isLive, node, say, setLocal, wide} from "./dom.js";
import {canStartCall, renderCurrent, select, startPeerCall} from "./live.js";
import {state} from "./state.js";

let browserTerminal = getLocal("lark-terminal");
if (!/^web-[0-9a-f]{8}$/.test(browserTerminal)) {
  const bytes = crypto.getRandomValues(new Uint8Array(4));
  browserTerminal = "web-" + Array.from(bytes, b => b.toString(16).padStart(2, "0")).join("");
  setLocal("lark-terminal", browserTerminal);
}

function renderSidebar() {
  $("sidebar").hidden = !state.app?.sidebar;
  $("workspace").dataset.sidebar = state.app?.sidebar ? "open" : "closed";
  $("sidebar-toggle").setAttribute("aria-expanded", String(!!state.app?.sidebar));
}

function primaryTerminal(c) {
  const terms = c?.terminals || [];
  return terms.find(t => t.online) || terms[0] || null;
}

function renderMoveTerminal() {
  const termSelect = $("move-terminal-select");
  const compSelect = $("move-target");
  const hw = (state.app.terminals || []).filter(t => t.kind === "hardware");
  const comps = state.app.companions || [];
  const termVer = JSON.stringify(hw.map(t => [t.id, t.companion_id]));
  if (termSelect.dataset.choices !== termVer) {
    const prev = termSelect.value;
    termSelect.replaceChildren(new Option("Choose a terminal…", ""),
      ...hw.map(t => new Option(`${t.id}${t.companion_id ? ` (${t.companion_id})` : ""}`, t.id)));
    termSelect.value = hw.some(t => t.id === prev) ? prev : "";
    termSelect.dataset.choices = termVer;
  }
  const compVer = JSON.stringify(comps.map(c => [c.id, companionName(c)]));
  if (compSelect.dataset.choices !== compVer) {
    const prev = compSelect.value;
    compSelect.replaceChildren(new Option("Choose a companion…", ""),
      ...comps.map(c => new Option(companionName(c), c.id)));
    compSelect.value = comps.some(c => c.id === prev) ? prev : "";
    compSelect.dataset.choices = compVer;
  }
  const selectedTerm = hw.find(t => t.id === termSelect.value);
  termSelect.disabled = !!state.app.moving || !hw.length;
  compSelect.disabled = !!state.app.moving || !comps.length;
  $("move-companion").disabled = !!state.app.moving || !selectedTerm || !compSelect.value || selectedTerm.companion_id === compSelect.value;
}

async function moveHardwareTerminal() {
  const current = state.app, termID = $("move-terminal-select").value, to = $("move-target").value;
  if (!current || current.scope || current.moving || !termID || !to) return;
  current.moving = true;
  renderMoveTerminal();
  say("reassign-note", "Moving terminal…");
  try {
    await api(current, `/api/terminals/${encodeURIComponent(termID)}/attach`, {method:"POST", body:{companion_id:to}});
    if (state.app !== current) return;
    say("reassign-note", `Moved ${termID} to ${to}.`);
    refreshDevices();
    loadHistory();
  } catch {
    if (state.app === current) say("reassign-note", "Could not move terminal.");
  } finally {
    current.moving = false;
    if (state.app === current) renderMoveTerminal();
  }
}
$("move-terminal-select").onchange = renderMoveTerminal;
$("move-target").onchange = renderMoveTerminal;
$("move-companion").onclick = moveHardwareTerminal;

$("sidebar-toggle").onclick = () => { state.app.sidebar = !state.app.sidebar; renderSidebar(); };
wide.addEventListener("change", e => {
  if (state.app && !state.app.scope) { state.app.sidebar = e.matches; renderSidebar(); }
});

const rowParts = new WeakMap();
function companionRow(c) {
  const row = node("div", "device-row");
  row.dataset.id = c.id;
  const button = node("button", "device");
  button.type = "button";
  const call = node("button", "outline peer-call", "Call");
  call.type = "button";
  call.onclick = () => startPeerCall(row.dataset.id);
  const name = node("span", "device-name"), status = node("span", "badge device-state");
  const meta = node("span", "device-meta"), badge = node("span", "badge outline", "HTTPS Tunnel");
  const dot = node("span", "companion-dot");
  dot.setAttribute("aria-hidden", "true");
  button.append(dot, name, status, meta, badge);
  row.append(button, call);
  rowParts.set(row, {button, call, name, status, meta, badge});
  button.onclick = () => {
    const selected = (state.app.companions || []).find(item => item.id === row.dataset.id);
    if (selected) {
      select(selected, false);
      if (!wide.matches) {
        state.app.sidebar = false;
        renderSidebar();
        $("sidebar-toggle").focus();
      }
    }
  };
  return row;
}

function renderDevices() {
  if (!state.app || state.app.scope) return;
  const nav = $("companions");
  const list = state.app.companions || [];
  const rows = new Map([...nav.children].filter(row => row.dataset.id).map(row => [row.dataset.id, row]));
  nav.querySelector("p")?.remove();
  for (const c of list) {
    const row = rows.get(c.id) ?? companionRow(c);
    rows.delete(c.id);
    const {button, call, name, status, meta, badge} = rowParts.get(row);
    colors(button, c);
    button.setAttribute("aria-pressed", String(state.view?.id === c.id));
    const canCall = (c.terminals || []).some(t => t.online && t.caps?.call);
    call.hidden = !state.view?.web || !canCall || state.view.id === c.id;
    call.disabled = !canStartCall();
    const label = companionName(c);
    call.setAttribute("aria-label", `Call ${label}`);
    if (name.textContent !== label) name.textContent = label;
    const live = isLive(c);
    status.setAttribute("data-variant", live ? "success" : c.dormant ? "secondary" : "warning");
    status.textContent = live ? "Live" : c.dormant ? "Dormant" : "Offline";
    const terms = c.terminals || [];
    meta.textContent = terms.map(t => {
      const parts = [t.id];
      if (t.online && t.mode && t.mode !== "offline") parts.push(t.mode);
      if (Number.isFinite(t.battery_pct)) parts.push(`${t.battery_pct}%`);
      return parts.join(" · ");
    }).join(", ");
    meta.hidden = !meta.textContent;
    badge.hidden = !c.tunnel?.active;
    if (!row.isConnected) nav.append(row);
  }
  for (const stale of rows.values()) stale.remove();
  if (!list.length) nav.append(node("p", "small", "No companions yet."));
  renderTargets(list);
  renderTunnel();
  renderMoveTerminal();
}

let callTargets = new Set();
function renderCallTarget() {
  $("call-target").disabled = !canStartCall() || !callTargets.has($("target").value);
}
$("target").onchange = renderCallTarget;
$("call-target").onclick = () => startPeerCall($("target").value);

function renderTargets(companions) {
  const target = $("target");
  const choices = (companions || []).filter(c => c.online && c.id !== state.view?.id);
  callTargets = new Set(choices.filter(c => c.caps?.call || (c.terminals || []).some(t => t.online && t.caps?.call)).map(c => c.id));
  const values = choices.map(c => `${c.id}:${companionName(c)}`).join("\n");
  if (target.dataset.choices !== values) {
    const prior = target.value;
    target.replaceChildren(new Option("Ask my companion", ""), ...choices.map(c => new Option(companionName(c), c.id)));
    target.value = choices.some(c => c.id === prior) ? prior : "";
    target.dataset.choices = values;
  }
  renderCallTarget();
}

const preferredCompanion = companions => companions.find(c => isLive(c)) || companions[0] || null;

async function refreshDevices() {
  const current = state.app;
  if (!current || current.scope) return;
  if (current.companionRequest) { current.companionsDirty = true; return; }
  const version = current.companionVersion;
  current.companionRequest = true;
  try {
    const [{companions}, {terminals}] = await Promise.all([
      api(current, "/api/companions"),
      api(current, "/api/terminals"),
    ]);
    if (state.app !== current || version !== current.companionVersion) return;
    current.companions = companions;
    current.terminals = terminals;
    const updated = companions.find(c => c.id === state.view?.id);
    if (!state.view?.id || !updated && !current.tunnelBusy) {
      const requested = companions.find(c => c.id === current.historyCompanion);
      select(requested || preferredCompanion(companions), false);
      current.historyCompanion = null;
    } else if (updated) {
      state.view.companion = updated;
      renderCurrent();
    }
    renderDevices();
    if ($("device-note").textContent === "Could not load companions.") say("device-note", "");
  } catch {
    if (state.app === current) say("device-note", "Could not load companions.");
  } finally {
    current.companionRequest = false;
    if (state.app === current && current.companionsDirty) {
      current.companionsDirty = false;
      refreshDevices();
    }
  }
}

async function refreshTunnel() {
  const current = state.app;
  if (!current || current.scope || current.tunnelRequest) return;
  current.tunnelRequest = true;
  try {
    const {installed, service_running} = await api(current, "/api/tunnel/status");
    current.tunnel = {installed, service_running};
  } catch {
    current.tunnel = null;
  } finally {
    current.tunnelRequest = false;
    if (state.app === current) renderTunnel();
  }
}

function renderTunnel() {
  const status = state.app.tunnel;
  say("tunnel-status", !status ? "Service status unavailable." : !status.installed ? "CLI not installed." :
    status.service_running ? "Service running." : "Service not running.");
  $("install-note").hidden = status?.installed !== false;
  const c = state.view?.companion, show = !!c?.id && !state.app.scope, lock = !!state.app.tunnelBusy;
  $("tunnel-controls").hidden = !show;
  $("tunnel-hint").hidden = show;
  const tunnel = show ? c.tunnel : null;
  const share = tunnel?.active ? safeShareURL(tunnel.share_url) : "";
  if ($("share-url").value !== share) {
    $("share-url").value = share;
    say("copy", "Copy Share Link");
    if (share) $("open-link").href = share;
    else $("open-link").removeAttribute("href");
  }
  if (!show) return;
  say("tunnel-companion", companionName(c));
  $("open-here").textContent = state.view?.web ? "Switch to mirror" : "Open companion here";
  $("open-here").setAttribute("aria-pressed", String(!!state.view?.web));
  $("share").hidden = !share;
  $("tunnel-create").hidden = !!tunnel?.active;
  $("tunnel-create").disabled = lock || (!status?.installed && !tunnel);
  say("tunnel-create", tunnel ? "Restore Tunnel" : "Create Tunnel");
  $("tunnel-close").hidden = !tunnel;
  $("tunnel-close").disabled = lock;
  const hasTerminals = (c.terminals || []).length > 0;
  $("delete-companion").disabled = lock || hasTerminals;
  $("delete-cancel").hidden = !state.view.confirmDelete;
  $("delete-cancel").disabled = lock;
  say("delete-companion", state.view.confirmDelete ? "Confirm delete" : "Delete companion");
}

function safeShareURL(link) {
  try {
    const url = new URL(link);
    return url.protocol === "https:" && !url.username && !url.password ? url.href : "";
  } catch { return ""; }
}

$("create-companion").onclick = async e => {
  const current = state.app, button = e.currentTarget;
  button.disabled = true;
  say("device-note", "Creating companion…");
  try {
    const created = await api(current, "/api/companions", {method:"POST", body:{}});
    if (state.app !== current) return;
    current.companionVersion++;
    current.companions.push(created);
    say("device-note", "Say hi to meet your new companion!");
    unlockAudio();
    select(created, true);
    if (!wide.matches) {
      current.sidebar = false;
      renderSidebar();
      $("sidebar-toggle").focus();
    }
    refreshDevices();
  } catch {
    if (state.app === current) say("device-note", "Could not create a companion.");
  } finally {
    button.disabled = false;
  }
};

$("open-here").onclick = () => {
  const c = state.view?.companion;
  if (!c) return;
  const nextWeb = !state.view.web;
  if (nextWeb) unlockAudio();
  const prev = state.view;
  state.view = null;
  select(c, nextWeb);
  if (prev && state.view) state.view.confirmDelete = false;
  if (!wide.matches) {
    state.app.sidebar = false;
    renderSidebar();
    $("sidebar-toggle").focus();
  }
};

async function changeTunnel(action) {
  const current = state.app, view = state.view, id = encodeURIComponent(view.id);
  if (current.tunnelBusy) return;
  current.tunnelBusy = true;
  renderTunnel();
  say("tunnel-note", "Working…");
  try {
    const create = action === "create";
    const result = await api(current, create ? "/api/tunnels" :
      action === "delete" ? `/api/companions/${id}` : `/api/tunnels/${id}`,
    create ? {method:"POST", body:{companion_id:view.id}} : {method:"DELETE"});
    if (state.app !== current) return;
    current.companionVersion++;
    const c = current.companions.find(item => item.id === view.id);
    if (action === "delete") {
      current.companions = current.companions.filter(item => item.id !== view.id);
      if (state.view === view) select(preferredCompanion(current.companions), false);
      say("device-note", "Companion deleted.");
    } else if (c) {
      c.tunnel = result;
      if (state.view === view) state.view.companion = c;
    }
    if (state.view === view) say("tunnel-note", create ? "Tunnel created." : "Tunnel closed.");
    refreshDevices();
    refreshTunnel();
  } catch {
    if (state.app === current && state.view === view) say("tunnel-note", `Could not ${action} ${action === "delete" ? "the companion" : "the tunnel"}.`);
  } finally {
    current.tunnelBusy = false;
    if (state.app === current) renderDevices();
  }
}

$("tunnel-create").onclick = () => changeTunnel("create");
$("tunnel-close").onclick = () => changeTunnel("close");
$("delete-companion").onclick = () => {
  if (state.view.confirmDelete) changeTunnel("delete");
  else {
    state.view.confirmDelete = true;
    say("tunnel-note", "Delete this companion? Its share link and history will be removed.");
    renderTunnel();
  }
};
$("delete-cancel").onclick = () => {
  state.view.confirmDelete = false;
  say("tunnel-note", "");
  renderTunnel();
  $("delete-companion").focus();
};

async function copyField(field) {
  try {
    await navigator.clipboard.writeText(field.value);
    return true;
  } catch {
    field.focus();
    field.select();
    try { return document.execCommand("copy"); } catch { return false; }
  }
}

$("copy").onclick = async () => {
  const field = $("share-url"), value = field.value;
  const copied = await copyField(field);
  if (field.value !== value) return;
  say("copy", copied ? "Copied!" : "Copy Share Link");
  say("tunnel-note", copied ? "Share link copied." : "Copy the selected link with your keyboard.");
  if (copied) setTimeout(() => { if (field.value === value) say("copy", "Copy Share Link"); }, 2000);
};

const quickVoices = [$("voice-first"), $("voice-second")];
function loadVoiceCatalog(voices) {
  $("voice-select").replaceChildren(...voices.map(voice => new Option(voice.name, voice.id)));
  for (const [i, button] of quickVoices.entries()) {
    button.hidden = !voices[i];
    button.value = voices[i]?.id ?? "";
    button.textContent = voices[i]?.name ?? "";
    button.onclick = () => setVoice(button.value);
  }
}

function renderVoice(companion, pending) {
  $("voice-controls").hidden = !companion;
  const voice = pending || companion?.voice || "";
  for (const button of quickVoices) {
    button.disabled = !!pending;
    button.setAttribute("aria-pressed", String(voice === button.value));
  }
  $("voice-select").disabled = !!pending;
  $("voice-select").value = voice;
}

async function setVoice(voice) {
  const cat = state.view, current = state.app;
  if (!cat?.id || cat.voicePending || cat.companion.voice === voice) return;
  cat.voicePending = voice;
  say("voice-note", "");
  renderVoice(cat.companion, voice);
  try {
    const result = await api(current, `/api/companions/${encodeURIComponent(cat.id)}/voice`, {method:"POST", body:{voice}});
    cat.companion.voice = result.voice;
    current.companionVersion++;
    const comp = (current.companions || []).find(c => c.id === cat.id);
    if (comp) comp.voice = result.voice;
  } catch {
    if (state.view === cat) say("voice-note", "Could not save voice.");
  } finally {
    cat.voicePending = null;
    if (state.view === cat) renderVoice(cat.companion);
  }
}
$("voice-select").onchange = e => setVoice(e.target.value);

$("volume").oninput = e => { $("volume-value").value = e.target.value + "%"; };
$("volume").onchange = e => saveVolume({auto:false, volume:+e.target.value});
$("volume-auto").onclick = () => {
  const t = primaryTerminal(state.view?.companion);
  if (t?.auto_volume === false) saveVolume({auto:true});
};

function renderVolume() {
  const t = primaryTerminal(state.view?.companion);
  $("volume-auto").setAttribute("aria-pressed", String(t?.auto_volume !== false));
  $("volume-auto").disabled = $("volume").disabled = !t || !!state.view?.volumePending;
}

function applyVolume(current, cat, level, auto_volume) {
  const t = primaryTerminal(cat?.companion);
  if (!t || !Number.isFinite(level)) return;
  const fields = {volume:level, ...(typeof auto_volume === "boolean" && {auto_volume})};
  Object.assign(t, fields);
  current.companionVersion++;
  const comp = (current.companions || []).find(c => c.id === cat.id);
  const compTerm = primaryTerminal(comp);
  if (compTerm) Object.assign(compTerm, fields);
  if (state.view === cat) {
    setVolume(level);
    $("volume").value = String(level);
    $("volume-value").value = `${level}%`;
    renderVolume();
    renderDevices();
  }
}

async function saveVolume(settings) {
  const cat = state.view, current = state.app;
  const t = primaryTerminal(cat?.companion);
  if (!t || cat.volumePending) return;
  const previous = {level:t.volume ?? volume(), auto:t.auto_volume !== false};
  cat.volumePending = true;
  renderVolume();
  try {
    const result = await api(current, `/api/terminals/${encodeURIComponent(t.id)}/volume`, {method:"POST", body:settings});
    if (state.view === cat) applyVolume(current, cat, result.volume, result.auto_volume);
  } catch {
    if (state.view === cat) {
      applyVolume(current, cat, previous.level, previous.auto);
      say("chat-note", "Could not save volume.");
    }
  } finally {
    cat.volumePending = false;
    if (state.view === cat) renderVolume();
  }
}

export {applyVolume, browserTerminal, copyField, loadVoiceCatalog, refreshDevices, refreshTunnel, renderCallTarget, renderDevices, renderSidebar, renderTargets, renderVoice, renderVolume};
