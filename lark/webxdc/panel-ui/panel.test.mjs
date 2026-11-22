// Headless-Chromium tests for the webxdc panel. Dev-only: run with
// `./scripts/check.sh --panel-ui`. The page is served from a loopback HTTP
// server and `window.webxdc` is a deterministic shim; no Lark server, relay,
// or internet access is needed.
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const here = path.dirname(fileURLToPath(import.meta.url));
const panelDir = path.join(here, "..", "panel");
const require = createRequire(import.meta.url);

function loadPlaywright() {
  const tried = [];
  for (const id of [process.env.LARK_PLAYWRIGHT_MODULE, "playwright", "playwright-core"]) {
    if (!id) continue;
    tried.push(id);
    try { return require(id); } catch {}
  }
  throw new Error(`panel UI tests need playwright or playwright-core (tried: ${tried.join(", ")}); set LARK_PLAYWRIGHT_MODULE to its directory`);
}

function chromiumExecutable() {
  if (process.env.LARK_CHROMIUM) return process.env.LARK_CHROMIUM;
  const roots = [process.env.PLAYWRIGHT_BROWSERS_PATH, path.join(os.homedir(), ".cache", "ms-playwright")].filter(Boolean);
  const found = [];
  for (const root of roots) {
    let entries = [];
    try { entries = fs.readdirSync(root); } catch { continue; }
    for (const name of entries) {
      const m = /^chromium(?:_headless_shell)?-(\d+)$/.exec(name);
      if (!m) continue;
      const exe = name.startsWith("chromium_headless_shell")
        ? path.join(root, name, "chrome-headless-shell-linux64", "chrome-headless-shell")
        : path.join(root, name, "chrome-linux64", "chrome");
      if (fs.existsSync(exe)) found.push({ version: Number(m[1]), exe });
    }
  }
  found.sort((a, b) => b.version - a.version);
  return found[0]?.exe;
}

const PORTRAIT = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==";
const SHOTS = process.env.LARK_PANEL_SHOTS || "/tmp/opencode/panel";

function stateUpdate(over = {}, companion = {}, terminals) {
  return {
    v: 1,
    op: "state",
    companion: Object.assign({
      id: "greg",
      name: "greg",
      form: "an old chameleon",
      voice: "af_heart",
      listen_seconds: 0,
      live_state: "idle",
      portrait: PORTRAIT,
    }, companion),
    terminals: terminals || [{ id: "box3-b324", name: "BOX-3", online: true, volume: 60, battery_pct: 84, charging: true }],
    stories: [
      { name: "lost_island", title: "Lost Island", passages: 12, progress: 42 },
      { name: "clockwork_owl", title: "Clockwork Owl", passages: 8, progress: 0 },
    ],
    voices: [{ id: "af_heart", name: "Heart" }, { id: "bm_fable", name: "Fable" }],
    ...over,
  };
}

function rows(prefix, count, extra = {}) {
  return Array.from({ length: count }, (_, i) => ({
    id: `${prefix}${i}`,
    speaker: i % 2 ? "greg" : "Someone in person",
    channel: i % 2 ? "in_person" : "deltachat",
    text: `${prefix} message ${i}`,
    ts_ms: 1760000000000 + i * 1000,
    ...extra,
  }));
}

class Panel {
  constructor(page, errors) {
    this.page = page;
    this.errors = errors;
  }

  static async open(browser, url) {
    const errors = [];
    const page = await browser.newPage({ viewport: { width: 420, height: 820 } });
    page.on("pageerror", (err) => errors.push(err));
    await page.addInitScript(() => {
      const sent = [];
      let listener = null;
      window.__panel = {
        get sent() { return sent; },
        get listener() { return listener; },
        deliver(payload) {
          if (!listener) throw new Error("panel listener not registered");
          listener({ payload, serial: sent.length + 1, max_serial: sent.length + 1 });
        },
      };
      window.webxdc = {
        selfAddr: "tester@example.org",
        selfName: "Tester",
        sendUpdate(update) { sent.push(update.payload ?? update); },
        setUpdateListener(cb) { listener = cb; return Promise.resolve(); },
      };
    });
    await page.goto(url);
    await page.waitForFunction(() => window.__panel && window.__panel.listener);
    const panel = new Panel(page, errors);
    await panel.deliver(stateUpdate());
    return panel;
  }

  deliver(payload) {
    return this.page.evaluate((p) => window.__panel.deliver(p), payload);
  }

  sent() {
    return this.page.evaluate(() => window.__panel.sent.map((u) => u && u.payload ? u.payload : u));
  }

  async lastSent(op) {
    const sent = await this.sent();
    for (let i = sent.length - 1; i >= 0; i--) if (sent[i].op === op) return sent[i];
    return null;
  }

  async waitSent(op, pred = () => true) {
    const deadline = Date.now() + 3000;
    while (Date.now() < deadline) {
      const found = await this.lastSent(op);
      if (found && pred(found)) return found;
      await this.page.waitForTimeout(20);
    }
    throw new Error(`no ${op} update was sent`);
  }

  async tab(name) {
    await this.page.click(`#tab-${name}`);
    await this.page.waitForFunction((n) => {
      const btn = document.getElementById("tab-" + n);
      const panel = document.getElementById("panel-" + n);
      return btn.getAttribute("aria-selected") === "true" && !panel.hasAttribute("hidden");
    }, name);
  }

  async answerHistory(request, items, extra = {}) {
    await this.deliver({
      v: 1,
      op: "history",
      dir: "out",
      sid: request.sid,
      seq: request.seq,
      channel: request.channel,
      q: request.q || "",
      append: !!extra.append,
      has_more: false,
      items,
      ...extra,
    });
  }

  async close() {
    assert.deepEqual(this.errors.map((e) => e.message), [], "panel page errors");
    await this.page.close();
  }

  shot(name) {
    fs.mkdirSync(SHOTS, { recursive: true });
    return this.page.screenshot({ path: path.join(SHOTS, `${name}.png`), fullPage: true });
  }
}

let server, browser, baseURL;

before(async () => {
  const { chromium } = loadPlaywright();
  server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://127.0.0.1");
    if (url.pathname === "/webxdc.js") {
      // Real clients inject this file; the shim lives in an init script.
      res.writeHead(200, { "content-type": "text/javascript" });
      res.end("// webxdc shim injected by ui.test.mjs\n");
      return;
    }
    const file = path.normalize(path.join(panelDir, url.pathname === "/" ? "index.html" : url.pathname));
    if (!file.startsWith(panelDir) || !fs.existsSync(file)) {
      res.writeHead(404);
      res.end();
      return;
    }
    const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".png": "image/png" };
    res.writeHead(200, { "content-type": types[path.extname(file)] || "application/octet-stream" });
    fs.createReadStream(file).pipe(res);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseURL = `http://127.0.0.1:${server.address().port}/index.html`;
  browser = await chromium.launch({ headless: true, executablePath: chromiumExecutable() });
  fs.mkdirSync(SHOTS, { recursive: true });
});

after(async () => {
  await browser?.close();
  await new Promise((resolve) => server?.close(resolve));
});

function open() {
  return Panel.open(browser, baseURL);
}

test("header shows the companion portrait, name, form, and live state", async () => {
  const p = await open();
  try {
    assert.equal(await p.page.textContent("#comp-name"), "greg");
    assert.equal(await p.page.textContent("#comp-form"), "an old chameleon");
    assert.equal(await p.page.title(), "greg");
    const img = p.page.locator("#portrait-wrap img.portrait");
    assert.equal(await img.count(), 1);
    assert.equal(await img.getAttribute("src"), PORTRAIT);
    assert.equal(await img.getAttribute("alt"), "greg portrait");
    assert.equal(await p.page.getAttribute("#live-state", "data-state"), "idle");
    assert.equal(await p.page.textContent("#live-state-text"), "Idle");

    await p.deliver({ v: 1, op: "live_state", dir: "out", live_state: "listening" });
    assert.equal(await p.page.getAttribute("#live-state", "data-state"), "listening");
    assert.equal(await p.page.textContent("#live-state-text"), "Listening");
    assert.equal(await p.page.textContent("#comp-name"), "greg");
    await p.shot("status");
  } finally {
    await p.close();
  }
});

test("Say posts, shows feedback, and listening and volume controls send the right values", async () => {
  const p = await open();
  try {
    await p.tab("status");
    const label = p.page.locator('label[for="say-input"]');
    assert.equal(await label.count(), 1);
    await p.page.fill("#say-input", "Hello home");
    await p.page.click('#body-status button[type="submit"]');
    const say = await p.waitSent("say");
    assert.equal(say.text, "Hello home");
    assert.equal(await p.page.getAttribute("#feedback", "data-visible"), "true");
    assert.equal(await p.page.textContent("#feedback"), "Speaking on device…");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), "true");

    await p.deliver({ v: 1, op: "ack", dir: "out", req_op: "say", ok: true, message: "Said it." });
    assert.equal(await p.page.textContent("#feedback"), "Said it.");
    assert.equal(await p.page.getAttribute("#feedback", "data-tone"), "ok");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), null);

    await p.page.click('#body-status .pill-btn:has-text("2m")');
    const listen = await p.waitSent("set_listen");
    assert.equal(listen.seconds, 120);
    await p.deliver(stateUpdate({}, { listen_seconds: 120 }));
    const pressed = p.page.locator('#body-status .pill-btn[aria-pressed="true"]');
    assert.equal((await pressed.allInnerTexts()).join(","), "2m");

    const slider = p.page.locator('input[type="range"][aria-label="Volume for BOX-3"]');
    await slider.fill("75");
    const vol = await p.waitSent("set_volume");
    assert.deepEqual({ terminal: vol.terminal, volume: vol.volume }, { terminal: "box3-b324", volume: 75 });
    await p.deliver(stateUpdate({}, {}, [{ id: "box3-b324", name: "BOX-3", online: true, volume: 75 }]));
    assert.equal(await slider.inputValue(), "75");
    assert.equal(await p.page.textContent("#body-status .range-row span"), "75%");
  } finally {
    await p.close();
  }
});

test("Stories show progress and send open and start-over commands", async () => {
  const p = await open();
  try {
    await p.tab("stories");
    const cards = p.page.locator("#body-stories article");
    assert.equal(await cards.count(), 2);
    assert.equal(await cards.nth(0).locator("progress").getAttribute("value"), "42");
    assert.equal(await cards.nth(1).locator("progress").getAttribute("value"), "0");
    assert.match(await cards.nth(0).innerText(), /In progress · 42%/);
    assert.match(await cards.nth(1).innerText(), /Not started/);

    await cards.nth(0).locator('button:has-text("Continue on device")').click();
    const openStory = await p.waitSent("open_story");
    assert.equal(openStory.app, "lost_island");
    await cards.nth(0).locator('button:has-text("Start over")').click();
    const restart = await p.waitSent("restart_story");
    assert.equal(restart.app, "lost_island");
    assert.equal(await cards.nth(1).locator('button:has-text("Start over")').count(), 0);
    assert.equal(await cards.nth(1).locator('button:has-text("Open on device")').count(), 1);
    await p.shot("stories");
  } finally {
    await p.close();
  }
});

test("History lists rows, pages, filters, and searches", async () => {
  const p = await open();
  try {
    await p.tab("history");
    const req1 = await p.waitSent("history");
    assert.equal(req1.channel, "all");
    assert.equal(req1.limit, 20);
    assert.ok(req1.sid && req1.seq === 1);
    await p.answerHistory(req1, rows("first", 3), {
      has_more: true,
      next_cursor: { ts_ms: 1759999999000, id: "first2" },
    });
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);
    assert.match(await p.page.locator("#body-history .hist-item").first().innerText(), /first message 0/);

    await p.page.click('#body-history button:has-text("Load more")');
    const req2 = await p.waitSent("history", (r) => r.before_id === "first2");
    assert.equal(req2.before_ts, 1759999999000);
    assert.equal(req2.sid, req1.sid);
    assert.equal(req2.seq, 2);
    await p.answerHistory(req2, rows("second", 1), { append: true });
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 4);
    assert.equal(await p.page.locator('#body-history button:has-text("Load more")').count(), 0);

    await p.page.fill("#hist-search", "weather");
    await p.page.click('#body-history button:has-text("Find")');
    const req3 = await p.waitSent("history", (r) => r.q === "weather");
    await p.answerHistory(req3, [{ id: "w1", speaker: "Someone in person", channel: "in_person", text: "How is the weather?", ts_ms: 1760000001000 }]);
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 1);
    assert.match(await p.page.locator("#body-history .hist-item").innerText(), /How is the weather\?/);
    assert.match(await p.page.locator("#body-history .hist-item").innerText(), /In person/);

    await p.page.click('#body-history .pill-btn:has-text("Intercom")');
    const req4 = await p.waitSent("history", (r) => r.channel === "intercom");
    await p.answerHistory(req4, []);
    assert.match(await p.page.locator("#body-history").innerText(), /No matching messages/);

    await p.page.click('#body-history .pill-btn:has-text("All")');
    const req5 = await p.waitSent("history", (r) => r.channel === "all" && r.seq > req4.seq);
    await p.answerHistory(req5, rows("back", 2));
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 2);
    await p.shot("history");
  } finally {
    await p.close();
  }
});

// Regression: a live_state/state push or a replayed history response used to
// replace the list (often with an empty page) while History was open.
test("regression: history survives state pushes, tab switches, and stale responses", async () => {
  const p = await open();
  try {
    await p.tab("history");
    const req1 = await p.waitSent("history");
    await p.answerHistory(req1, rows("page1", 2), { has_more: true, next_cursor: { ts_ms: 1759999998000, id: "page1-1" } });
    await p.page.click('#body-history button:has-text("Load more")');
    const req2 = await p.waitSent("history", (r) => r.seq === 2);
    await p.answerHistory(req2, rows("page2", 1), { append: true });
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);

    await p.deliver({ v: 1, op: "live_state", dir: "out", live_state: "speaking" });
    await p.deliver(stateUpdate({}, { voice: "bm_fable" }));
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);

    await p.tab("status");
    await p.tab("history");
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);

    // Replayed responses from an older panel instance / other filter.
    await p.deliver({ v: 1, op: "history", dir: "out", sid: "other-panel", seq: 1, channel: "intercom", q: "", has_more: false, append: false, items: [] });
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);
    // A superseded seq for this instance.
    await p.deliver({ v: 1, op: "history", dir: "out", sid: req2.sid, seq: 1, channel: "all", q: "", has_more: false, append: false, items: [] });
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);
    assert.doesNotMatch(await p.page.locator("#body-history").innerText(), /No matching messages/);
    await p.shot("history-regression");
  } finally {
    await p.close();
  }
});

// Regression: reopening a panel replays old status updates. A replayed
// history response must not count as a fresh load, or the tab shows an old
// filter's page (often empty) and never fetches again.
test("regression: a replayed history response does not satisfy the first load", async () => {
  const p = await open();
  try {
    await p.deliver({ v: 1, op: "history", dir: "out", sid: "old-panel", seq: 1, channel: "intercom", q: "", has_more: false, append: false, items: [] });
    await p.tab("history");
    const req = await p.waitSent("history");
    assert.notEqual(req.sid, "old-panel");
    await p.answerHistory(req, rows("fresh", 2));
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 2);
    assert.doesNotMatch(await p.page.locator("#body-history").innerText(), /No matching messages/);
  } finally {
    await p.close();
  }
});

// Regression: the set_voice ack carries the saved voice, so the pressed state
// updates immediately instead of waiting for (or losing) a later state push.
test("regression: voice selection updates from the ack without a state push", async () => {
  const p = await open();
  try {
    await p.tab("settings");
    const pressed = () => p.page.locator(".voice-btn[aria-pressed=true]").allInnerTexts();
    assert.deepEqual(await pressed(), ["Heart\nActive"]);

    await p.page.click('.voice-btn:has-text("Fable")');
    const req = await p.waitSent("set_voice");
    assert.equal(req.voice, "bm_fable");
    await p.deliver({ v: 1, op: "ack", dir: "out", req_op: "set_voice", ok: true, message: "Voice updated.", voice: "bm_fable" });
    assert.deepEqual(await pressed(), ["Fable\nActive"]);
    assert.equal(await p.page.locator(".voice-btn .row-sub").count(), 1);

    await p.deliver({ v: 1, op: "live_state", dir: "out", live_state: "idle" });
    assert.deepEqual(await pressed(), ["Fable\nActive"]);
    await p.shot("settings");
  } finally {
    await p.close();
  }
});

test("tabs support keyboard navigation and controls are labelled", async () => {
  const p = await open();
  try {
    assert.equal(await p.page.getAttribute('nav[role="tablist"]', "aria-label"), "Panel sections");
    assert.equal(await p.page.locator('.tab-btn[role="tab"]').count(), 4);
    await p.page.focus("#tab-status");
    await p.page.keyboard.press("ArrowRight");
    assert.equal(await p.page.getAttribute("#tab-stories", "aria-selected"), "true");
    assert.equal(await p.page.evaluate(() => document.activeElement.id), "tab-stories");
    await p.page.keyboard.press("End");
    assert.equal(await p.page.getAttribute("#tab-settings", "aria-selected"), "true");
    await p.page.keyboard.press("Home");
    assert.equal(await p.page.getAttribute("#tab-status", "aria-selected"), "true");

    for (const id of ["tab-status", "tab-stories", "tab-history", "tab-settings"]) {
      assert.ok((await p.page.textContent("#" + id)).trim().length > 0);
    }
    await p.page.focus("#tab-status");
    await p.page.keyboard.press("Tab");
    assert.equal(await p.page.evaluate(() => document.activeElement.id), "say-input");
    await p.tab("history");
    assert.equal(await p.page.locator('label[for="hist-search"]').count(), 1);
    assert.equal(await p.page.getAttribute('#body-history .pill-group', "aria-label"), "Filter by channel");
    await p.tab("settings");
    assert.equal(await p.page.getAttribute("#body-settings .voice-grid", "aria-label"), "Voice selection");
  } finally {
    await p.close();
  }
});
