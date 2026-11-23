(function () {
  "use strict";

  const token = document.body.dataset.token || new URLSearchParams(location.search).get("token") || "";
  const tabs = ["status", "stories", "history", "settings"];
  const stateLabels = { idle: "Idle", listening: "Listening", thinking: "Thinking", speaking: "Speaking", sleeping: "Sleeping", offline: "Offline" };
  const chanLabels = { in_person: "In person", deltachat: "Delta Chat", intercom: "Intercom", timer: "Timer" };
  const LISTEN_OPTIONS = [
    { seconds: 0, label: "Off" }, { seconds: 30, label: "30s" },
    { seconds: 60, label: "1m" }, { seconds: 120, label: "2m" }, { seconds: 300, label: "5m" },
  ];
  const HISTORY_CHANNELS = [
    { id: "all", label: "All" }, { id: "in_person", label: "In person" },
    { id: "deltachat", label: "Delta Chat" }, { id: "intercom", label: "Intercom" },
  ];

  let state = { companion: {}, terminals: [], stories: [], voices: [] };
  let hist = { loaded: false, loading: false, channel: "all", q: "", items: [], has_more: false, next_cursor: null, seq: 0 };
  let bannerTimer = null, pendingBtn = null, reconnecting = false;

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

  function applyState(prev, next) {
    if (!next) return prev || { companion: {}, terminals: [], stories: [], voices: [] };
    const oldComp = (prev && prev.companion) || {};
    const companion = Object.assign({}, oldComp, next.companion || {});
    if (!next.companion || !next.companion.portrait) {
      if (oldComp.portrait) companion.portrait = oldComp.portrait;
    }
    return {
      companion,
      terminals: Array.isArray(next.terminals) ? next.terminals : [],
      stories: Array.isArray(next.stories) ? next.stories : [],
      voices: Array.isArray(next.voices) ? next.voices : [],
    };
  }

  function apiURL(path, params) {
    const url = new URL(path, location.origin);
    url.searchParams.set("token", token);
    if (params) for (const [k, v] of Object.entries(params)) if (v !== null && v !== undefined && v !== "") url.searchParams.set(k, String(v));
    return url.pathname + url.search;
  }

  async function call(path, body) {
    const init = { method: body === undefined ? "GET" : "POST", headers: { "X-Lark-Remote-Token": token } };
    if (body !== undefined) {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const res = await fetch(apiURL("/remote/api/" + path), init);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || "Request failed.");
    if (data.state) state = applyState(state, data.state);
    return data;
  }

  async function runCommand(path, body, pendingMsg, btn) {
    clearPending();
    if (btn) { pendingBtn = btn; btn.classList.add("is-pending"); btn.setAttribute("aria-busy", "true"); }
    if (pendingMsg) showBanner(pendingMsg, "info");
    try {
      const data = await call(path, body);
      showBanner(data.message || "Done.", "ok");
      clearPending();
      renderAll();
      return data;
    } catch (err) {
      showBanner(err.message || "Failed.", "error");
    } finally {
      clearPending();
    }
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

  function keyed(parent, selector, key, make) {
    let node = parent.querySelector(`${selector}[data-key="${CSS.escape(key)}"]`);
    if (!node) { node = make(); node.dataset.key = key; parent.appendChild(node); }
    return node;
  }

  function prune(parent, selector, keys) {
    for (const node of [...parent.querySelectorAll(selector)]) {
      if (!keys.has(node.dataset.key)) node.remove();
    }
  }

  function renderStatus() {
    const root = document.getElementById("body-status");
    let quick = root.querySelector("section.status-quick");
    if (!quick) {
      root.replaceChildren();
      const sayInput = el("input", { id: "say-input", type: "text", placeholder: "Say something aloud…", maxlength: 500 });
      const sayBtn = el("button", { type: "submit", text: "Say" });
      const sayForm = el("form", { class: "inline-form", onsubmit: (e) => {
        e.preventDefault();
        const text = sayInput.value.trim();
        if (!text) return;
        sayInput.value = "";
        runCommand("say", { text }, "Speaking on device…", sayBtn);
      } }, [el("label", { for: "say-input", class: "visually-hidden", text: "Words to say aloud" }), sayInput, sayBtn]);
      const pills = el("div", { class: "pill-group", role: "group", "aria-label": "Listening duration" });
      for (const opt of LISTEN_OPTIONS) {
        const btn = el("button", {
          type: "button", class: "pill-btn", "aria-pressed": "false", text: opt.label,
          onclick: () => runCommand("listen", { seconds: opt.seconds }, `Listening: ${opt.label}…`, btn),
        });
        btn.dataset.seconds = String(opt.seconds);
        pills.appendChild(btn);
      }
      quick = el("section", { class: "card-box status-quick" }, [
        el("div", { class: "row-title", text: "Quick actions" }), sayForm,
        el("div", { class: "row-sub", text: "Listening mode" }), pills,
        el("div", { id: "volume-rows", class: "volume-rows" }),
      ]);
      root.append(quick, el("p", { class: "section-label", text: "Devices" }), el("div", { id: "device-list", class: "panel-list" }));
    }
    const currentSec = Number(state.companion.listen_seconds || 0);
    for (const btn of quick.querySelectorAll(".pill-btn")) {
      btn.setAttribute("aria-pressed", Number(btn.dataset.seconds) === currentSec ? "true" : "false");
    }
    const volumes = quick.querySelector("#volume-rows");
    const terminalKeys = new Set(state.terminals.map((t) => t.id));
    prune(volumes, ".volume-row", terminalKeys);
    for (const t of state.terminals) {
      const row = keyed(volumes, ".volume-row", t.id, () => {
        const valLabel = el("span", { class: "row-sub" });
        const slider = el("input", { type: "range", min: 0, max: 100, step: 5, "aria-label": `Volume for ${t.name || t.id}` });
        slider.addEventListener("input", () => { valLabel.textContent = `${slider.value}%`; });
        slider.addEventListener("change", () => runCommand("volume", { terminal: t.id, volume: Number(slider.value) }, "Setting volume…", slider.form || undefined));
        return el("div", { class: "volume-row", "data-terminal": t.id }, [
          el("div", { class: "row-sub", text: `Volume · ${t.name || t.id}` }), el("div", { class: "range-row" }, [slider, valLabel]),
        ]);
      });
      const slider = row.querySelector('input[type="range"]');
      const valLabel = row.querySelector(".row-sub + .range-row .row-sub");
      slider.setAttribute("aria-label", `Volume for ${t.name || t.id}`);
      if (document.activeElement !== slider && !row.querySelector(".is-pending")) slider.value = t.volume ?? 60;
      if (valLabel) valLabel.textContent = `${slider.value}%`;
    }
    const list = root.querySelector("#device-list");
    prune(list, ".row-card", terminalKeys);
    if (!state.terminals.length) {
      if (!list.querySelector(".empty")) list.replaceChildren(el("p", { class: "empty", text: "No devices attached." }));
      return;
    }
    list.querySelector(".empty")?.remove();
    for (const t of state.terminals) {
      const card = keyed(list, ".row-card", t.id, () => el("article", { class: "row-card" }, [
        el("div", { class: "row-info" }, [el("div", { class: "row-title" }), el("div", { class: "row-sub" })]),
        el("span", { class: "state-pill" }, [el("span", { class: "state-dot", "aria-hidden": "true" }), el("span")]),
      ]));
      const parts = [t.online ? "Online" : "Offline"];
      if (typeof t.battery_pct === "number" && t.battery_pct >= 0) parts.push(`${t.battery_pct}%${t.charging ? " charging" : ""}`);
      card.querySelector(".row-title").textContent = t.name || t.id;
      card.querySelector(".row-sub").textContent = parts.join(" · ");
      const pill = card.querySelector(".state-pill");
      pill.dataset.state = t.online ? "idle" : "offline";
      pill.lastElementChild.textContent = t.online ? "Ready" : "Away";
    }
  }

  function renderStories() {
    const root = document.getElementById("body-stories");
    let list = root.querySelector(".panel-list.stories");
    if (!list) {
      root.replaceChildren();
      list = el("div", { class: "panel-list stories" });
      root.appendChild(list);
    }
    const keys = new Set(state.stories.map((s) => s.name));
    prune(list, "article.story-card", keys);
    list.querySelector(".empty")?.remove();
    if (!state.stories.length) {
      if (!list.querySelector(".empty")) list.appendChild(el("p", { class: "empty", text: "No stories saved yet." }));
      return;
    }
    for (const s of state.stories) {
      const card = keyed(list, "article.story-card", s.name, () => {
        const openBtn = el("button", { type: "button", class: "btn-compact", onclick: () => runCommand("story/open", { story: s.name }, `Opening ${s.title || s.name}…`, openBtn) });
        return el("article", { class: "card-box story-card" }, [
          el("div", { class: "row-info" }, [el("div", { class: "row-title" }), el("div", { class: "row-sub" })]),
          el("progress", { class: "story-bar", max: 100 }),
          el("div", { class: "btn-row" }, [openBtn]),
        ]);
      });
      const pct = Number(s.progress || 0);
      const status = pct >= 100 ? "Finished" : pct > 0 ? `In progress · ${pct}%` : "Not started";
      const title = s.title || s.name;
      card.querySelector(".row-title").textContent = title;
      card.querySelector(".row-sub").textContent = `${s.passages ? s.passages + " passages · " : ""}${status}`;
      const bar = card.querySelector("progress");
      bar.value = pct;
      bar.setAttribute("aria-label", `${title} progress: ${pct}%`);
      card.querySelector(".btn-row button").textContent = pct > 0 ? "Continue on device" : "Open on device";
      let resetBtn = card.querySelector(".story-reset");
      if (pct > 0 && !resetBtn) {
        resetBtn = el("button", { type: "button", class: "btn-compact btn-ghost story-reset", text: "Start over" });
        resetBtn.addEventListener("click", () => runCommand("story/restart", { story: s.name }, `Starting ${title} over…`, resetBtn));
        card.querySelector(".btn-row").appendChild(resetBtn);
      } else if (pct === 0 && resetBtn) {
        resetBtn.remove();
      }
    }
  }

  async function loadHistory(append) {
    hist.loading = true;
    hist.seq += 1;
    const seq = hist.seq;
    const params = { channel: hist.channel };
    if (hist.q) params.q = hist.q;
    params.limit = 20;
    if (append && hist.next_cursor) params.cursor = hist.next_cursor;
    renderHistory();
    try {
      const data = await call("history?" + new URLSearchParams(params).toString());
      if (seq !== hist.seq) return;
      hist.loaded = true;
      hist.loading = false;
      hist.has_more = !!data.has_more;
      hist.next_cursor = data.next_cursor || null;
      hist.items = append ? hist.items.concat(data.items || []) : (data.items || []);
      renderHistory();
    } catch (err) {
      if (seq !== hist.seq) return;
      hist.loading = false;
      renderHistory();
      showBanner(err.message || "Could not load history.", "error");
    }
  }

  function renderHistory() {
    const root = document.getElementById("body-history");
    let controls = root.querySelector("section.hist-controls");
    if (!controls) {
      root.replaceChildren();
      const chips = el("div", { class: "pill-group", role: "group", "aria-label": "Filter by channel" });
      for (const ch of HISTORY_CHANNELS) {
        const btn = el("button", { type: "button", class: "pill-btn", "aria-pressed": "false", text: ch.label });
        btn.dataset.channel = ch.id;
        btn.addEventListener("click", () => { hist.channel = ch.id; loadHistory(false); });
        chips.appendChild(btn);
      }
      const qInput = el("input", { id: "hist-search", type: "search", placeholder: "Search history…", maxlength: 100 });
      const searchForm = el("form", { class: "inline-form", onsubmit: (e) => { e.preventDefault(); hist.q = qInput.value.trim(); loadHistory(false); } }, [
        el("label", { for: "hist-search", class: "visually-hidden", text: "Search history" }), qInput, el("button", { type: "submit", text: "Find" }),
      ]);
      controls = el("section", { class: "card-box hist-controls" }, [chips, searchForm]);
      root.append(controls, el("ul", { class: "hist-list" }), el("div", { id: "hist-footer" }));
    }
    for (const btn of controls.querySelectorAll(".pill-btn")) {
      btn.setAttribute("aria-pressed", hist.channel === btn.dataset.channel ? "true" : "false");
    }
    const qInput = controls.querySelector("#hist-search");
    if (document.activeElement !== qInput && qInput.value !== hist.q) qInput.value = hist.q;
    const list = root.querySelector(".hist-list");
    const keys = new Set(hist.items.map((item) => item.id));
    prune(list, ".hist-item", keys);
    for (const item of hist.items) {
      const li = keyed(list, ".hist-item", item.id, () => el("li", { class: "hist-item" }, [
        el("div", { class: "hist-meta" }, [el("span", { class: "hist-speaker" }), el("span")]),
        el("div", { class: "hist-text" }),
      ]));
      list.appendChild(li);
      const when = item.ts_ms ? new Date(item.ts_ms).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : "";
      li.querySelector(".hist-speaker").textContent = item.speaker || "Unknown";
      li.querySelector(".hist-meta span:last-child").textContent = [chanLabels[item.channel] || item.channel, when].filter(Boolean).join(" · ");
      li.querySelector(".hist-text").textContent = item.text || "";
    }
    const footer = root.querySelector("#hist-footer");
    let foot = footer.firstElementChild;
    if (hist.loading && !hist.items.length) {
      if (!foot || !foot.classList.contains("empty")) footer.replaceChildren(el("p", { class: "empty", text: "Loading history…" }));
    } else if (!hist.items.length) {
      if (!foot || !foot.classList.contains("empty")) footer.replaceChildren(el("p", { class: "empty", text: "No matching messages." }));
    } else if (hist.has_more) {
      if (!foot || foot.tagName !== "BUTTON") {
        const more = el("button", { type: "button", class: "btn-compact btn-ghost", onclick: () => loadHistory(true) });
        footer.replaceChildren(more);
        foot = more;
      }
      foot.textContent = hist.loading ? "Loading…" : "Load more";
    } else {
      footer.replaceChildren();
    }
  }

  function renderSettings() {
    const root = document.getElementById("body-settings");
    let section = root.querySelector("section.voice-section");
    if (!section) {
      root.replaceChildren();
      section = el("section", { class: "card-box voice-section" }, [
        el("div", { class: "row-title", text: "Voice" }), el("div", { class: "row-sub", text: "Tap a voice to save and preview it on the device." }),
        el("div", { class: "voice-grid", role: "group", "aria-label": "Voice selection" }),
      ]);
      const revoke = el("button", { type: "button", class: "btn-compact btn-ghost", text: "Forget this link" });
      revoke.addEventListener("click", () => {
        runCommand("revoke", {}, "Turning off remote controls…", revoke).then((data) => { if (data && data.ok) window.location.href = "/remote"; });
      });
      root.append(section, el("section", { class: "card-box" }, [
        el("div", { class: "row-title", text: "This link" }),
        el("div", { class: "row-sub", text: "Ask the companion for /remote again to get a fresh link." }), revoke,
      ]));
    }
    const grid = section.querySelector(".voice-grid");
    const keys = new Set(state.voices.map((v) => v.id));
    prune(grid, ".voice-btn", keys);
    for (const v of state.voices) {
      const btn = keyed(grid, ".voice-btn", v.id, () => el("button", { type: "button", class: "voice-btn", onclick: () => runCommand("voice", { voice: v.id }, `Previewing ${v.name}…`, btn) }, [
        el("span", { class: "voice-name" }),
      ]));
      const active = v.id === state.companion.voice;
      btn.querySelector(".voice-name").textContent = v.name;
      btn.setAttribute("aria-pressed", active ? "true" : "false");
      let label = btn.querySelector(".row-sub");
      if (active && !label) btn.appendChild(el("span", { class: "row-sub", text: "Active" }));
      else if (!active && label) label.remove();
    }
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
    if (name === "history" && !hist.loaded && !hist.loading) loadHistory(false);
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

  if (!token) {
    showBanner("This remote link is missing its token. Ask the companion for /remote again.", "error");
    return;
  }

  const source = new EventSource(apiURL("/remote/events"));
  source.addEventListener("state", (event) => {
    const next = JSON.parse(event.data);
    state = applyState(state, next);
    reconnecting = false;
    renderAll();
  });
  source.onerror = () => {
    if (reconnecting) return;
    reconnecting = true;
    showBanner("Reconnecting to the companion…", "info");
  };
  const q = new URLSearchParams(location.search).get("tab");
  if (tabs.includes(q)) switchTab(q, false);
})();
