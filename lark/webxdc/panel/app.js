(function () {
  "use strict";

  const codec = window.LarkPanelCodec;
  const tabs = ["status", "books", "games", "settings"];
  let activeTab = "status";
  let state = { companion: {}, terminals: [], books: [], games: [], voices: [] };
  let bannerTimer = null;

  function el(tag, attrs, children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (k === "text") node.textContent = v;
        else if (k === "class") node.className = v;
        else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
        else if (v !== null && v !== undefined) node.setAttribute(k, String(v));
      }
    }
    if (children) {
      for (const c of children) {
        if (c) node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
      }
    }
    return node;
  }

  function showBanner(msg, tone) {
    const b = document.getElementById("feedback");
    b.textContent = msg;
    b.dataset.tone = tone || "info";
    b.dataset.visible = "true";
    clearTimeout(bannerTimer);
    bannerTimer = setTimeout(() => { b.dataset.visible = "false"; }, 4000);
  }

  function sendOp(op, fields, pendingMsg) {
    const payload = codec.encodeCommand(op, fields);
    if (pendingMsg) showBanner(pendingMsg, "info");
    if (window.webxdc && typeof window.webxdc.sendUpdate === "function") {
      window.webxdc.sendUpdate({ payload }, pendingMsg || op);
    }
  }

  function renderHeader() {
    const c = state.companion || {};
    document.getElementById("comp-name").textContent = c.name || "Lark Companion";
    document.getElementById("comp-form").textContent = c.form || "Waiting for status…";
    const wrap = document.getElementById("portrait-wrap");
    if (c.portrait && !wrap.querySelector("img")) {
      wrap.className = "";
      wrap.innerHTML = "";
      wrap.appendChild(el("img", { class: "portrait", src: c.portrait, alt: c.name || "Companion portrait" }));
    }
  }

  function renderStatus() {
    const root = document.getElementById("panel-status");
    root.innerHTML = "";
    const sec = Number(state.companion.listen_seconds || 0);
    const listenText = sec > 0 ? `On (${sec >= 60 ? sec / 60 + "m" : sec + "s"})` : "Off";
    root.appendChild(el("article", { class: "row-card" }, [
      el("div", { class: "row-info" }, [
        el("div", { class: "row-title", text: "Listening mode" }),
        el("div", { class: "row-sub", text: listenText }),
      ]),
      el("span", { class: "badge", text: sec > 0 ? "Listening" : "Tap to talk" }),
    ]));

    if (!state.terminals.length) {
      root.appendChild(el("p", { class: "empty", text: "No terminals attached." }));
      return;
    }
    for (const t of state.terminals) {
      const parts = [t.online ? "Online" : "Offline"];
      if (typeof t.battery_pct === "number" && t.battery_pct >= 0) {
        parts.push(`${t.battery_pct}%${t.charging ? " ⚡" : ""}`);
      }
      root.appendChild(el("article", { class: "row-card" }, [
        el("div", { class: "row-info" }, [
          el("div", { class: "row-title", text: t.name || t.id }),
          el("div", { class: "row-sub", text: parts.join(" · ") }),
        ]),
        el("span", { class: "badge", text: t.online ? "Ready" : "Away" }),
      ]));
    }
  }

  function renderAppList(rootId, items, op, verb, emptyText) {
    const root = document.getElementById(rootId);
    root.innerHTML = "";
    if (!items.length) {
      root.appendChild(el("p", { class: "empty", text: emptyText }));
      return;
    }
    for (const item of items) {
      const btn = el("button", {
        type: "button",
        class: "button",
        text: verb,
        onclick: () => sendOp(op, { app: item.name }, `${verb}: ${item.title || item.name}…`),
      });
      root.appendChild(el("article", { class: "row-card" }, [
        el("div", { class: "row-info" }, [
          el("div", { class: "row-title", text: item.title || item.name }),
          el("div", { class: "row-sub", text: item.name }),
        ]),
        btn,
      ]));
    }
  }

  function renderSettings() {
    const root = document.getElementById("panel-settings");
    root.innerHTML = "";

    const voiceBox = el("article", { class: "row-card field-stack" }, [
      el("div", { class: "row-title", text: "Voice" }),
    ]);
    const select = el("select", { "aria-label": "Companion voice" });
    for (const v of state.voices) {
      const opt = el("option", { value: v.id, text: v.name });
      if (v.id === state.companion.voice) opt.selected = true;
      select.appendChild(opt);
    }
    select.addEventListener("change", () => sendOp("set_voice", { voice: select.value }, "Updating voice…"));
    voiceBox.appendChild(select);
    root.appendChild(voiceBox);

    const listenCard = el("article", { class: "row-card field-stack" }, [
      el("div", { class: "row-title", text: "Listening mode" }),
    ]);
    const pills = el("div", { class: "pill-group" });
    const currentSec = Number(state.companion.listen_seconds || 0);
    for (const opt of codec.LISTEN_OPTIONS) {
      pills.appendChild(el("button", {
        type: "button",
        class: "pill-btn",
        "aria-pressed": opt.seconds === currentSec ? "true" : "false",
        text: opt.label,
        onclick: () => sendOp("set_listen", { seconds: opt.seconds }, `Setting listening to ${opt.label}…`),
      }));
    }
    listenCard.appendChild(pills);
    root.appendChild(listenCard);

    for (const t of state.terminals) {
      const valLabel = el("span", { class: "row-sub", text: `${t.volume ?? 60}%` });
      const slider = el("input", {
        type: "range", min: 0, max: 100, step: 5,
        value: t.volume ?? 60,
        "aria-label": `Volume for ${t.name || t.id}`,
      });
      slider.addEventListener("input", () => { valLabel.textContent = `${slider.value}%`; });
      slider.addEventListener("change", () => sendOp("set_volume", { terminal: t.id, volume: Number(slider.value) }, "Setting volume…"));
      root.appendChild(el("article", { class: "row-card field-stack" }, [
        el("div", { class: "row-title", text: `Volume · ${t.name || t.id}` }),
        el("div", { class: "range-row" }, [slider, valLabel]),
      ]));
    }
  }

  function renderAll() {
    renderHeader();
    renderStatus();
    renderAppList("panel-books", state.books, "open_book", "Open on device", "No books saved yet.");
    renderAppList("panel-games", state.games, "play_game", "Play on device", "No games saved yet.");
    renderSettings();
  }

  function switchTab(name) {
    activeTab = name;
    for (const btn of document.querySelectorAll(".tab-btn")) {
      btn.setAttribute("aria-selected", btn.dataset.tab === name ? "true" : "false");
    }
    for (const id of tabs) {
      document.getElementById("panel-" + id).hidden = id !== name;
    }
  }

  for (const btn of document.querySelectorAll(".tab-btn")) {
    btn.addEventListener("click", () => switchTab(btn.dataset.tab));
  }

  if (window.webxdc && typeof window.webxdc.setUpdateListener === "function") {
    window.webxdc.setUpdateListener((update) => {
      const msg = codec.decodeUpdate(update && update.payload);
      if (!msg) return;
      if (msg.op === "state") {
        state = codec.applyStateUpdate(state, msg);
        renderAll();
      } else if (msg.op === "ack") {
        showBanner(msg.message || (msg.ok ? "Done." : "Failed."), msg.ok ? "ok" : "error");
      }
    }, 0).then(() => sendOp("sync"));
  } else {
    state = codec.applyStateUpdate(state, {
      v: 1,
      op: "state",
      companion: { id: "greg", name: "Greg", form: "an old chameleon", voice: "bf_lily", listen_seconds: 60 },
      terminals: [{ id: "box3-b324", name: "BOX-3", online: true, volume: 60 }],
      books: [{ name: "lost_island", title: "Lost Island" }, { name: "clockwork_owl", title: "The Clockwork Owl" }],
      games: [{ name: "math_game", title: "Math Game" }, { name: "fireworks", title: "Fireworks" }],
      voices: [{ id: "af_heart", name: "Heart" }, { id: "bf_lily", name: "Lily" }, { id: "bm_fable", name: "Fable" }],
    });
    renderAll();
    const q = new URLSearchParams(location.search).get("tab");
    if (tabs.includes(q)) switchTab(q);
  }
})();
