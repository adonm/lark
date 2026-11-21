import {api, initialToken, onUnauthorized, request} from "./api.js";
import {onAudioActivityChanged} from "./audio.js";
import {copyField, loadVoiceCatalog, refreshDevices, refreshTunnel, renderDevices, renderSidebar} from "./devices.js";
import {setDiagnostics} from "./diag.js";
import {$, node, say, setLocal, wide} from "./dom.js";
import {tickTimers} from "./library.js";
import {disconnect, renderCurrent, select} from "./live.js";
import {state} from "./state.js";

let photoLibrary;

function credentials(note = "") {
  say("auth-note", note);
  $("token-cancel").hidden = !state.app;
  if (!$("credentials").open) $("credentials").showModal();
}

function stop() {
  photoLibrary?.setPhotoLibrary(false);
  setDiagnostics(false);
  $("diagnostics-toggle").hidden = true;
  clearInterval(state.poll);
  clearInterval(state.timerTick);
  state.view?.loading?.abort();
  state.view = state.app = null;
  disconnect();
  renderCurrent();
  $("workspace").hidden = true;
  $("lan-actions").hidden = true;
  $("sidebar-toggle").hidden = true;
  $("parent-link").hidden = true;
  say("settings", "Bridge token");
  $("opencode-pairing").hidden = true;
  $("pair-links").replaceChildren();
  say("pair-note", "");
  say("link-state", "Not connected");
}

async function authorize(token) {
  $("connect").disabled = true;
  say("auth-note", "Checking token…");
  try {
    const res = await request(token, "/api/scope");
    if (res.status === 401 || res.status === 403) {
      if (!state.app) setLocal("lark-token", "");
      credentials("That token wasn’t accepted. Check it and try again.");
      return;
    }
    const profile = res.ok ? await res.json() : null;
    if (typeof profile?.scoped !== "boolean" || profile.scoped && !profile.device_id || !Array.isArray(profile.voices)) {
      throw new Error("Unexpected scope response");
    }
    const scope = profile?.scoped ? profile : null;
    stop();
    setLocal("lark-token", token);
    state.app = {token, scope, devices:[], deviceVersion:0, sidebar:!scope && wide.matches, tunnel:null};
    if (!scope) state.app.historyDevice = new URL(location.href).searchParams.get("device_id");
    loadVoiceCatalog(profile.voices);
    state.timerTick = setInterval(tickTimers, 1000);
    $("workspace").dataset.app = scope ? "scoped" : "lan";
    $("workspace").hidden = false;
    $("lan-actions").hidden = !!scope;
    $("diagnostics-toggle").hidden = !!scope;
    $("sidebar-toggle").hidden = !!scope;
    $("parent-link").hidden = !!scope;
    say("settings", scope ? "Access" : "Bridge token");
    $("opencode-pairing").hidden = !!scope;
    $("credentials").close();
    $("token").value = "";
    renderSidebar();
    if (scope) {
      $("share-url").value = "";
      $("open-link").removeAttribute("href");
      select({...scope, kind:"virtual"});
    } else {
      say("link-state", "Looking for companions…");
      renderDevices();
      select(null);
      refreshDevices();
      refreshTunnel();
      state.poll = setInterval(() => { refreshDevices(); refreshTunnel(); }, 5000);
      const current = state.app;
      import("./photo-library.js").then(module => {
        photoLibrary = module;
        if (state.app === current) module.setPhotoLibrary(true);
      }).catch(() => {});
    }
  } catch {
    credentials("Could not reach the bridge. Check the connection and try again.");
  } finally {
    $("connect").disabled = false;
  }
}

$("settings").onclick = () => credentials();
$("token-cancel").onclick = () => $("credentials").close();
$("credentials").oncancel = e => { if (!state.app || $("connect").disabled) e.preventDefault(); };
$("token-form").onsubmit = e => {
  e.preventDefault();
  const token = $("token").value.trim();
  if (token) authorize(token);
};

$("pair-opencode").onclick = async e => {
  const current = state.app, button = e.currentTarget, list = $("pair-links");
  if (!current || current.scope) return;
  button.disabled = true;
  list.replaceChildren();
  say("pair-note", "Creating one-time link…");
  try {
    const result = await api(current, "/api/opencode/pair", {method:"POST"});
    if (state.app !== current) return;
    if (!result.ok || !Array.isArray(result.links) || !result.links.length) throw new Error();
    const links = result.links.map(({label, url}) => {
      const target = new URL(url);
      if (target.protocol !== "http:" || target.username || target.password || target.search || target.hash ||
          !/^\/auth\/connect\/[A-Za-z0-9_-]+$/.test(target.pathname)) throw new Error();
      const group = node("div", "pair-link");
      const field = node("input");
      field.type = "text";
      field.value = target.href;
      field.readOnly = true;
      field.spellcheck = false;
      field.setAttribute("aria-label", `${label} pairing link`);
      const actions = node("div", "row wrap");
      const copy = node("button", "outline", "Copy");
      copy.type = "button";
      copy.onclick = async () => {
        const copied = await copyField(field);
        if (state.app === current && list.contains(field)) {
          say("pair-note", copied ? "Pairing link copied." : "Copy the selected link with your keyboard.");
        }
      };
      const open = node("a", "button outline", "Open ↗");
      open.href = target.href;
      open.target = "_blank";
      open.rel = "noopener noreferrer";
      actions.append(copy, open);
      group.append(field, actions);
      return group;
    });
    list.replaceChildren(...links);
    say("pair-note", "Use a link once before it expires.");
  } catch {
    if (state.app === current) say("pair-note", "Could not pair OpenCode. Check the dedicated service and your LAN connection.");
  } finally {
    button.disabled = false;
  }
};

onUnauthorized(() => {
  stop();
  setLocal("lark-token", "");
  credentials("Your token is no longer accepted.");
});
onAudioActivityChanged(renderCurrent);

function updateAtmosphere() {
  const now = new Date();
  const m = now.getHours() * 60 + now.getMinutes();
  document.documentElement.dataset.tod = m < 330 || m >= 1230 ? "night"
    : m < 510 ? "dawn" : m < 1020 ? "day" : "dusk";
}
updateAtmosphere();
setInterval(updateAtmosphere, 15 * 60 * 1000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) updateAtmosphere();
});

credentials();
if (initialToken) authorize(initialToken);
