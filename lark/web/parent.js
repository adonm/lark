const $ = id => document.getElementById(id);
const auth = $("parent-auth"), controls = $("parent-controls"), status = $("parent-note");
const note = text => { status.textContent = text; };
let access, pending = false;

try { $("bridge-token").value = localStorage.getItem("lark-token") || ""; } catch {}

async function request(path, fields) {
  const headers = {"X-Lark-Token": access.bridge, "X-Lark-Parent-Token": access.parent, Accept:"text/html"};
  const method = fields ? "POST" : "GET";
  const failed = (text = "") => {
    try { return JSON.parse(text).error; } catch { return "Cannot reach parent controls."; }
  };
  const active = document.activeElement;
  const id = controls.contains(active) ? active.id : "";
  if (window.htmx) {
    const abort = new AbortController();
    let error = "Cannot reach parent controls.";
    status.addEventListener("htmx:after:request", e => {
      if (e.detail.ctx.response.status === 200) error = "";
      else {
        error = failed(e.detail.ctx.text);
        e.preventDefault();
      }
    }, {signal:abort.signal});
    try {
      // A source containing both forms would serialize fields from both.
      await window.htmx.ajax(method, path, {source:status, target:controls, values:fields, headers,
        swap:"innerHTML settle:0"});
      if (error) throw new Error(error);
    } finally { abort.abort(); }
  } else {
    // Keep LAN administration available without an internet connection.
    const response = await fetch(path, {headers:{...headers, "HX-Request":"true"}, cache:"no-store",
      method, body:fields && new URLSearchParams(fields)});
    const text = await response.text();
    if (!response.ok) throw new Error(failed(text));
    const fragment = new DOMParser().parseFromString(text, "text/html");
    controls.replaceChildren(...fragment.body.childNodes);
  }
  if (id && document.activeElement === document.body) $(id)?.focus();
}

function overview(id = "") {
  return request(`/api/parent${id ? `?companion_id=${encodeURIComponent(id)}` : ""}`);
}

async function work(action, success) {
  if (pending) return;
  pending = true;
  note("");
  try {
    await action();
    note(success);
  } catch (err) {
    note(err.message || "Cannot reach parent controls.");
  } finally { pending = false; }
}

auth.onsubmit = e => {
  e.preventDefault();
  work(async () => {
    access = {bridge:$("bridge-token").value.trim(), parent:$("parent-token").value.trim()};
    $("parent-token").value = "";
    try { await overview(); }
    catch (err) { access = null; $("parent-token").focus(); throw err; }
    auth.hidden = true;
    controls.hidden = false;
    ($("parent-companion").disabled ? $("parent-refresh") : $("parent-companion")).focus();
  }, "");
};

controls.onchange = e => {
  if (e.target.id === "parent-companion") work(() => overview(e.target.value), "");
};
controls.onclick = e => {
  if (e.target.id === "parent-refresh") work(() => overview($("parent-companion").value), "Usage refreshed.");
  if (e.target.id === "parent-signout" && !pending) {
    access = null;
    controls.hidden = true;
    controls.replaceChildren();
    auth.hidden = false;
    note("Signed out of parent controls.");
    $("parent-token").focus();
  }
};
controls.onsubmit = e => {
  e.preventDefault();
  const id = $("parent-companion").value;
  const action = e.target.id === "parent-settings" ? "settings" : e.submitter?.value;
  if (!id || !action) return;
  const fields = action === "end_override" ? {} : Object.fromEntries(new FormData(e.target));
  work(() => request(`/api/devices/${encodeURIComponent(id)}/parent`, {action, ...fields}),
    {settings:"Rules saved.", end_override:"Override ended.", pause:"Companion paused.", unlock:"Companion unlocked."}[action]);
};
