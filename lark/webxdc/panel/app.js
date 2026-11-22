(function () {
  "use strict";

  const codec = window.LarkPanelCodec;
  const tabs = ["status", "stories", "history", "settings"];
  const stateLabels = { idle: "Idle", listening: "Listening", thinking: "Thinking", speaking: "Speaking", sleeping: "Sleeping", offline: "Offline" };
  const chanLabels = { in_person: "In person", deltachat: "Delta Chat", intercom: "Intercom", timer: "Timer" };
  let state = { companion: {}, terminals: [], stories: [], voices: [] };
  // sid identifies this panel instance. History responses are only rendered when
  // they answer a request from this instance, so replayed or cross-filter
  // responses cannot replace the list.
  const historySession = typeof crypto !== "undefined" && crypto.randomUUID
    ? crypto.randomUUID().replace(/-/g, "").slice(0, 12)
    : Math.random().toString(16).slice(2, 14);
  let hist = { loaded: false, loading: false, channel: "all", q: "", items: [], has_more: false, next_cursor: null, sid: historySession, seq: 0 };
  let bannerTimer = null, pendingBtn = null;

  function el(tag, attrs, children) {
    const node = document.createElement(tag);
    if (attrs) for (const [k, v] of Object.entries(attrs)) {
      if (k === "text") node.textContent = v;
      else if (k === "class") node.className = v;
      else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined) node.setAttribute(k, String(v));
    }
    if (children) for (const c of children) if (c) node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    return node;
  }

  function clearPending() {
    if (!pendingBtn) return;
    pendingBtn.classList.remove("is-pending");
    pendingBtn.removeAttribute("aria-busy");
    pendingBtn = null;
  }

  function showBanner(msg, tone) {
    const b = document.getElementById("feedback");
    b.textContent = msg;
    b.dataset.tone = tone || "info";
    b.dataset.visible = "true";
    clearTimeout(bannerTimer);
    bannerTimer = setTimeout(() => { b.dataset.visible = "false"; }, 4000);
  }

  function sendOp(op, fields, pendingMsg, btn) {
    let payload;
    try { payload = codec.encodeCommand(op, fields); }
    catch (err) { showBanner(err.message || "Invalid input.", "error"); return; }
    clearPending();
    if (btn) { pendingBtn = btn; btn.classList.add("is-pending"); btn.setAttribute("aria-busy", "true"); }
    if (pendingMsg) showBanner(pendingMsg, "info");
    if (window.webxdc && typeof window.webxdc.sendUpdate === "function") window.webxdc.sendUpdate({ payload }, pendingMsg || op);
  }

  function renderHeader() {
    const c = state.companion || {};
    const name = c.name || "Lark";
    document.title = name;
    document.getElementById("comp-name").textContent = name;
    document.getElementById("comp-form").textContent = c.form || "Waiting for status…";
    const live = c.live_state || (state.terminals.some((t) => t.online) ? "idle" : "offline");
    document.getElementById("live-state").dataset.state = live;
    document.getElementById("live-state-text").textContent = stateLabels[live] || live;
    const wrap = document.getElementById("portrait-wrap");
    let img = wrap.querySelector("img.portrait");
    if (c.portrait) {
      if (!img) {
        wrap.className = "";
        wrap.innerHTML = "";
        img = el("img", { class: "portrait", alt: name + " portrait" });
        wrap.appendChild(img);
      }
      if (img.getAttribute("src") !== c.portrait) img.src = c.portrait;
      img.alt = name + " portrait";
      wrap.removeAttribute("aria-hidden");
    } else if (img) {
      img.remove();
      wrap.className = "portrait-placeholder";
      wrap.setAttribute("aria-hidden", "true");
    }
  }

  function renderStatus() {
    const root = document.getElementById("body-status");
    root.innerHTML = "";
    const sayInput = el("input", { id: "say-input", type: "text", placeholder: "Say something aloud…", maxlength: 500 });
    const sayBtn = el("button", { type: "submit", text: "Say" });
    const sayForm = el("form", { class: "inline-form", onsubmit: (e) => {
      e.preventDefault();
      const text = sayInput.value.trim();
      if (!text) return;
      sendOp("say", { text }, "Speaking on device…", sayBtn);
      sayInput.value = "";
    } }, [el("label", { for: "say-input", class: "visually-hidden", text: "Words to say aloud" }), sayInput, sayBtn]);

    const pills = el("div", { class: "pill-group", role: "group", "aria-label": "Listening duration" });
    const currentSec = Number(state.companion.listen_seconds || 0);
    for (const opt of codec.LISTEN_OPTIONS) {
      const btn = el("button", {
        type: "button", class: "pill-btn", "aria-pressed": opt.seconds === currentSec ? "true" : "false", text: opt.label,
        onclick: () => sendOp("set_listen", { seconds: opt.seconds }, `Listening: ${opt.label}…`, btn),
      });
      pills.appendChild(btn);
    }
    const quick = el("section", { class: "card-box" }, [
      el("div", { class: "row-title", text: "Quick actions" }), sayForm,
      el("div", { class: "row-sub", text: "Listening mode" }), pills,
    ]);
    for (const t of state.terminals) {
      const valLabel = el("span", { class: "row-sub", text: `${t.volume ?? 60}%` });
      const slider = el("input", { type: "range", min: 0, max: 100, step: 5, value: t.volume ?? 60, "aria-label": `Volume for ${t.name || t.id}` });
      slider.addEventListener("input", () => { valLabel.textContent = `${slider.value}%`; });
      slider.addEventListener("change", () => sendOp("set_volume", { terminal: t.id, volume: Number(slider.value) }, "Setting volume…"));
      quick.appendChild(el("div", { class: "row-sub", text: `Volume · ${t.name || t.id}` }));
      quick.appendChild(el("div", { class: "range-row" }, [slider, valLabel]));
    }
    root.appendChild(quick);
    root.appendChild(el("p", { class: "section-label", text: "Devices" }));
    if (!state.terminals.length) { root.appendChild(el("p", { class: "empty", text: "No devices attached." })); return; }
    for (const t of state.terminals) {
      const parts = [t.online ? "Online" : "Offline"];
      if (typeof t.battery_pct === "number" && t.battery_pct >= 0) parts.push(`${t.battery_pct}%${t.charging ? " charging" : ""}`);
      root.appendChild(el("article", { class: "row-card" }, [
        el("div", { class: "row-info" }, [el("div", { class: "row-title", text: t.name || t.id }), el("div", { class: "row-sub", text: parts.join(" · ") })]),
        el("span", { class: "state-pill", "data-state": t.online ? "idle" : "offline" }, [el("span", { class: "state-dot", "aria-hidden": "true" }), el("span", { text: t.online ? "Ready" : "Away" })]),
      ]));
    }
  }

  function renderStories() {
    const root = document.getElementById("body-stories");
    root.innerHTML = "";
    if (!state.stories.length) { root.appendChild(el("p", { class: "empty", text: "No stories saved yet." })); return; }
    for (const s of state.stories) {
      const pct = Number(s.progress || 0);
      const status = pct >= 100 ? "Finished" : pct > 0 ? `In progress · ${pct}%` : "Not started";
      const openBtn = el("button", { type: "button", class: "btn-compact", text: pct > 0 ? "Continue on device" : "Open on device", onclick: () => sendOp("open_story", { app: s.name }, `Opening ${s.title || s.name}…`, openBtn) });
      const actions = [openBtn];
      if (pct > 0) {
        const resetBtn = el("button", { type: "button", class: "btn-compact btn-ghost", text: "Start over", onclick: () => sendOp("restart_story", { app: s.name }, `Starting ${s.title || s.name} over…`, resetBtn) });
        actions.push(resetBtn);
      }
      root.appendChild(el("article", { class: "card-box" }, [
        el("div", { class: "row-info" }, [el("div", { class: "row-title", text: s.title || s.name }), el("div", { class: "row-sub", text: `${s.passages ? s.passages + " passages · " : ""}${status}` })]),
        el("progress", { class: "story-bar", max: 100, value: pct, "aria-label": `${s.title || s.name} progress: ${pct}%` }),
        el("div", { class: "btn-row" }, actions),
      ]));
    }
  }

  function loadHistory(append) {
    hist.loading = true;
    hist.seq += 1;
    renderHistory();
    const fields = { channel: hist.channel, q: hist.q, limit: 20, sid: hist.sid, seq: hist.seq };
    if (append && hist.next_cursor) { fields.before_ts = hist.next_cursor.ts_ms; fields.before_id = hist.next_cursor.id; }
    sendOp("history", fields);
  }

  function renderHistory() {
    const root = document.getElementById("body-history");
    root.innerHTML = "";
    const chips = el("div", { class: "pill-group", role: "group", "aria-label": "Filter by channel" });
    for (const ch of codec.HISTORY_CHANNELS) {
      chips.appendChild(el("button", { type: "button", class: "pill-btn", "aria-pressed": hist.channel === ch.id ? "true" : "false", text: ch.label, onclick: () => { hist.channel = ch.id; loadHistory(false); } }));
    }
    const qInput = el("input", { id: "hist-search", type: "search", value: hist.q, placeholder: "Search history…", maxlength: 100 });
    const searchForm = el("form", { class: "inline-form", onsubmit: (e) => { e.preventDefault(); hist.q = qInput.value.trim(); loadHistory(false); } }, [
      el("label", { for: "hist-search", class: "visually-hidden", text: "Search history" }), qInput, el("button", { type: "submit", text: "Find" }),
    ]);
    root.appendChild(el("section", { class: "card-box" }, [chips, searchForm]));
    if (hist.loading && !hist.items.length) { root.appendChild(el("p", { class: "empty", text: "Loading history…" })); return; }
    if (!hist.items.length) { root.appendChild(el("p", { class: "empty", text: "No matching messages." })); return; }
    const list = el("ul", { class: "hist-list" });
    for (const item of hist.items) {
      const when = item.ts_ms ? new Date(item.ts_ms).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : "";
      list.appendChild(el("li", { class: "hist-item" }, [
        el("div", { class: "hist-meta" }, [el("span", { class: "hist-speaker", text: item.speaker || "Unknown" }), el("span", { text: [chanLabels[item.channel] || item.channel, when].filter(Boolean).join(" · ") })]),
        el("div", { class: "hist-text", text: item.text || "" }),
      ]));
    }
    root.appendChild(list);
    if (hist.has_more) root.appendChild(el("button", { type: "button", class: "btn-compact btn-ghost", text: hist.loading ? "Loading…" : "Load more", onclick: () => loadHistory(true) }));
  }

  function renderSettings() {
    const root = document.getElementById("body-settings");
    root.innerHTML = "";
    const grid = el("div", { class: "voice-grid", role: "group", "aria-label": "Voice selection" });
    for (const v of state.voices) {
      const active = v.id === state.companion.voice;
      const btn = el("button", { type: "button", class: "voice-btn", "aria-pressed": active ? "true" : "false", onclick: () => sendOp("set_voice", { voice: v.id }, `Previewing ${v.name}…`, btn) }, [el("span", { text: v.name }), active ? el("span", { class: "row-sub", text: "Active" }) : null]);
      grid.appendChild(btn);
    }
    root.appendChild(el("section", { class: "card-box" }, [el("div", { class: "row-title", text: "Voice" }), el("div", { class: "row-sub", text: "Tap a voice to save and preview it on the device." }), grid]));
  }

  function renderAll() { renderHeader(); renderStatus(); renderStories(); renderSettings(); }

  function switchTab(name, focusTab) {
    if (!tabs.includes(name)) return;
    for (const btn of document.querySelectorAll(".tab-btn")) {
      const sel = btn.dataset.tab === name;
      btn.setAttribute("aria-selected", sel ? "true" : "false");
      btn.tabIndex = sel ? 0 : -1;
      if (sel && focusTab) btn.focus();
    }
    for (const id of tabs) {
      const panel = document.getElementById("panel-" + id);
      if (id === name) panel.removeAttribute("hidden");
      else panel.setAttribute("hidden", "until-found");
    }
    if (name === "history" && !hist.loaded && !hist.loading) {
      if (window.webxdc && typeof window.webxdc.sendUpdate === "function") loadHistory(false);
      else renderHistory();
    }
  }

  const tabBtns = Array.from(document.querySelectorAll(".tab-btn"));
  tabBtns.forEach((btn, idx) => {
    btn.addEventListener("click", () => switchTab(btn.dataset.tab, false));
    btn.addEventListener("keydown", (e) => {
      const next = e.key === "ArrowRight" ? (idx + 1) % tabBtns.length : e.key === "ArrowLeft" ? (idx - 1 + tabBtns.length) % tabBtns.length : e.key === "Home" ? 0 : e.key === "End" ? tabBtns.length - 1 : -1;
      if (next >= 0) { e.preventDefault(); switchTab(tabBtns[next].dataset.tab, true); }
    });
  });
  for (const id of tabs) document.getElementById("panel-" + id).addEventListener("beforematch", () => switchTab(id, false));

  if (window.webxdc && typeof window.webxdc.setUpdateListener === "function") {
    window.webxdc.setUpdateListener((update) => {
      const msg = codec.decodeUpdate(update && update.payload);
      if (!msg) return;
      if (msg.op === "state" || msg.op === "live_state") {
        state = codec.applyStateUpdate(state, msg);
        if (msg.op === "live_state") renderHeader();
        else renderAll();
      } else if (msg.op === "history") {
        // Delta Chat also delivers the panel's own updates back to this
        // listener. Only accept the server's answer to this instance's latest
        // request: replayed updates, our own command, and responses to
        // superseded filters must not touch the list.
        if (msg.dir !== "out" || msg.sid !== hist.sid || Number(msg.seq) !== hist.seq) return;
        clearPending();
        hist.loaded = true; hist.loading = false; hist.has_more = !!msg.has_more; hist.next_cursor = msg.next_cursor || null;
        hist.items = msg.append ? hist.items.concat(msg.items || []) : (msg.items || []);
        renderHistory();
      } else if (msg.op === "ack") {
        clearPending();
        if (msg.req_op === "history" && !msg.ok) { hist.loading = false; renderHistory(); }
        if (typeof msg.voice === "string" && msg.voice) {
          const voice = msg.voice;
          if (voice !== state.companion.voice) {
            state = Object.assign({}, state, { companion: Object.assign({}, state.companion, { voice }) });
            renderSettings();
          }
        }
        showBanner(msg.message || (msg.ok ? "Done." : "Failed."), msg.ok ? "ok" : "error");
      }
    }, 0).then(() => sendOp("sync"));
  } else {
    state = codec.applyStateUpdate(state, {
      v: 1, op: "state", companion: { id: "greg", name: "Greg", form: "an old chameleon", voice: "bf_lily", listen_seconds: 60, live_state: "idle" },
      terminals: [{ id: "box3-b324", name: "BOX-3", online: true, volume: 60, battery_pct: 84, charging: true }],
      stories: [{ name: "lost_island", title: "Lost Island", passages: 12, progress: 42 }],
      voices: [{ id: "af_heart", name: "Heart" }, { id: "bf_lily", name: "Lily" }],
    });
    renderAll();
    const q = new URLSearchParams(location.search).get("tab");
    if (tabs.includes(q)) switchTab(q, false);
  }
})();
