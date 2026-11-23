// Headless-Chromium tests for the remote control page. Dev-only: run with
// `./scripts/check.sh --remote-ui`. The page is served from a loopback HTTP
// server; REST responses and the SSE stream are deterministic fakes, so no
// Lark server, tunnel, relay, or internet access is needed.
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createRequire } from "node:module";

const here = path.dirname(fileURLToPath(import.meta.url));
const pageDir = path.join(here, "..", "remote");
const require = createRequire(import.meta.url);

function loadPlaywright() {
  const tried = [];
  for (const id of [process.env.LARK_PLAYWRIGHT_MODULE, "playwright", "playwright-core"]) {
    if (!id) continue;
    tried.push(id);
    try { return require(id); } catch {}
  }
  throw new Error(`remote UI tests need playwright or playwright-core (tried: ${tried.join(", ")}); set LARK_PLAYWRIGHT_MODULE to its directory`);
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

const TOKEN = "testtoken";
const PORTRAIT = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==";
const SHOTS = process.env.LARK_REMOTE_SHOTS || "/tmp/opencode/remote";

function stateUpdate(over = {}, companion = {}, terminals) {
  return {
    companion: Object.assign({
      id: "greg", name: "greg", form: "an old chameleon", voice: "af_heart",
      listening: false, live_state: "idle", portrait: PORTRAIT,
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

let server, browser, baseURL, sseClients = [], requests = [], pending = [], current = stateUpdate();
let resolveNext, nextResolve = new Promise((r) => { resolveNext = r; });

function answer(index, body = {}, status = 200) {
  const entry = pending[index];
  if (!entry) throw new Error(`no pending request ${index}`);
  pending[index] = null;
  entry.res.writeHead(status, { "content-type": "application/json" });
  entry.res.end(JSON.stringify(body));
  entry.resolve();
}

function pushState(next) {
  current = next;
  const frame = `event: state\ndata: ${JSON.stringify(next)}\n\n`;
  for (const res of sseClients) res.write(frame);
}

function pushCapture(status) {
  const frame = `event: capture\ndata: ${JSON.stringify(status)}\n\n`;
  for (const res of sseClients) res.write(frame);
}

function imagePNG() {
  return Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==", "base64");
}

class Remote {
  constructor(page, errors) { this.page = page; this.errors = errors; }

  static async open(browser) {
    const errors = [];
    current = stateUpdate();
    const page = await browser.newPage({ viewport: { width: 420, height: 820 } });
    page.on("pageerror", (err) => errors.push(err));
    // Oat is pinned on a CDN in production; stub it so tests stay offline.
    await page.route("https://cdn.jsdelivr.net/**", (route) =>
      route.fulfill({ status: 200, contentType: "text/css", body: "/* oat stub */" }));
    await page.goto(`${baseURL}/remote?token=${TOKEN}`);
    await page.waitForFunction(() => document.querySelector("#comp-name").textContent === "greg");
    return new Remote(page, errors);
  }

  deliver(state) { pushState(state); }

  deliverCapture(status) { pushCapture(status); }

  async waitForPath(pred, timeout = 3000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const found = requests.find(pred);
      if (found) return found;
      await this.page.waitForTimeout(20);
    }
    throw new Error("timed out waiting for a request path");
  }

  async waitForRequest(pred, timeout = 3000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      for (let i = 0; i < requests.length; i++) {
        if (pending[i] && pred(requests[i])) return { index: i, request: requests[i] };
      }
      await this.page.waitForTimeout(20);
    }
    throw new Error("timed out waiting for a request");
  }

  answer(index, body, status) { answer(index, body, status); }

  async tab(name) {
    await this.page.click(`#tab-${name}`);
    await this.page.waitForFunction((n) => {
      const btn = document.getElementById("tab-" + n);
      const panel = document.getElementById("panel-" + n);
      return btn.getAttribute("aria-selected") === "true" && !panel.hasAttribute("hidden");
    }, name);
  }

  async close() {
    assert.deepEqual(this.errors.map((e) => e.message), [], "remote page errors");
    await this.page.close();
  }

  shot(name) {
    fs.mkdirSync(SHOTS, { recursive: true });
    return this.page.screenshot({ path: path.join(SHOTS, `${name}.png`), fullPage: true });
  }
}

before(async () => {
  const { chromium } = loadPlaywright();
  server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://127.0.0.1");
    const pathname = url.pathname;
    if (pathname === "/remote" || pathname === "/remote/") {
      const page = fs.readFileSync(path.join(pageDir, "index.html"), "utf8")
        .replaceAll("{{TOKEN}}", TOKEN).replaceAll("{{TOKEN_HTML}}", TOKEN).replaceAll("{{NAME}}", "greg");
      res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
      res.end(page);
      return;
    }
    if (pathname === "/remote/events") {
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache", connection: "keep-alive" });
      res.write("retry: 2000\n\n");
      sseClients.push(res);
      res.write(`event: state\ndata: ${JSON.stringify(current)}\n\n`);
      req.on("close", () => { sseClients = sseClients.filter((r) => r !== res); });
      return;
    }
    if (pathname.startsWith("/remote/pwa/")) {
      if (pathname.endsWith(".png")) { res.writeHead(200, { "content-type": "image/png" }); res.end(imagePNG()); return; }
      res.writeHead(200, { "content-type": "application/manifest+json" });
      res.end(JSON.stringify({ name: "greg", start_url: "/remote", icons: [{ src: "/remote/pwa/icon-192.png" }] }));
      return;
    }
    if (pathname.startsWith("/remote/api/")) {
      let raw = "";
      req.on("data", (chunk) => { raw += chunk; });
      req.on("end", () => {
        let body = null;
        try { body = raw ? JSON.parse(raw) : null; } catch { body = raw; }
        const index = requests.length;
        requests.push({ method: req.method, path: pathname, query: Object.fromEntries(url.searchParams), body, headers: req.headers });
        if (req.method === "GET" && pathname.endsWith(".wav")) {
          pending.push(null);
          res.writeHead(200, { "content-type": "audio/wav" });
          res.end();
          resolveNext();
          nextResolve = new Promise((r) => { resolveNext = r; });
          return;
        }
        pending.push({ res, resolve: () => {} });
        if (pathname === "/remote/api/history" && req.method === "GET") {
          // Tests answer history explicitly, like other commands.
        }
        resolveNext();
        nextResolve = new Promise((r) => { resolveNext = r; });
      });
      return;
    }
    const file = path.normalize(path.join(pageDir, pathname.replace(/^\/remote\//, "")));
    if (!file.startsWith(pageDir) || !fs.existsSync(file)) { res.writeHead(404); res.end(); return; }
    const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".png": "image/png" };
    res.writeHead(200, { "content-type": types[path.extname(file)] || "application/octet-stream" });
    fs.createReadStream(file).pipe(res);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseURL = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true, executablePath: chromiumExecutable() });
  fs.mkdirSync(SHOTS, { recursive: true });
});

after(async () => {
  await browser?.close();
  await new Promise((resolve) => server?.close(resolve));
});

function open() { return Remote.open(browser); }

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
    p.deliver(stateUpdate({}, { live_state: "listening" }));
    await p.page.waitForFunction(() => document.querySelector("#live-state-text").textContent === "Listening");
    assert.equal(await p.page.getAttribute("#live-state", "data-state"), "listening");
    await p.shot("status");
  } finally { await p.close(); }
});

test("Say posts, shows feedback, and listening and volume controls send the right values", async () => {
  const p = await open();
  try {
    await p.page.fill("#say-input", "Hello home");
    await p.page.click('#body-status button[type="submit"]');
    const say = await p.waitForRequest((r) => r.path === "/remote/api/say");
    assert.deepEqual(say.request.body, { text: "Hello home" });
    assert.equal(await p.page.getAttribute("#feedback", "data-visible"), "true");
    assert.equal(await p.page.textContent("#feedback"), "Speaking on device…");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), "true");
    p.answer(say.index, { ok: true, message: "Said it.", state: stateUpdate() });
    await p.page.waitForFunction(() => document.querySelector("#feedback").textContent === "Said it.");
    assert.equal(await p.page.getAttribute("#feedback", "data-tone"), "ok");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), null);

    await p.page.click('#body-status .pill-btn:has-text("Listen")');
    const listen = await p.waitForRequest((r) => r.path === "/remote/api/listen");
    assert.deepEqual(listen.request.body, { listening: true });
    p.answer(listen.index, { ok: true, message: "Listening mode updated.", state: stateUpdate({}, { listening: true }) });
    await p.page.waitForFunction(() => document.querySelector('#body-status .pill-btn[aria-pressed="true"]')?.textContent === "Listen");

    const slider = p.page.locator('input[type="range"][aria-label="Volume for BOX-3"]');
    await slider.fill("75");
    const vol = await p.waitForRequest((r) => r.path === "/remote/api/volume");
    assert.deepEqual(vol.request.body, { terminal: "box3-b324", volume: 75 });
    p.answer(vol.index, { ok: true, message: "Volume updated.", state: stateUpdate({}, {}, [{ id: "box3-b324", name: "BOX-3", online: true, volume: 75 }]) });
    await p.page.waitForFunction(() => document.querySelector('input[type="range"][aria-label="Volume for BOX-3"]').value === "75");
  } finally { await p.close(); }
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
    const openReq = await p.waitForRequest((r) => r.path === "/remote/api/story/open");
    assert.deepEqual(openReq.request.body, { story: "lost_island" });
    p.answer(openReq.index, { ok: true, message: "Opened on device.", state: stateUpdate() });

    await cards.nth(0).locator('button:has-text("Start over")').click();
    const restart = await p.waitForRequest((r) => r.path === "/remote/api/story/restart");
    assert.deepEqual(restart.request.body, { story: "lost_island" });
    p.answer(restart.index, { ok: true, message: "Started over on device.", state: stateUpdate() });
    assert.equal(await cards.nth(1).locator('button:has-text("Start over")').count(), 0);
    await p.shot("stories");
  } finally { await p.close(); }
});

test("History lists rows, pages, filters, and searches", async () => {
  const p = await open();
  try {
    await p.tab("history");
    const req1 = await p.waitForRequest((r) => r.path === "/remote/api/history");
    assert.equal(req1.request.query.channel, "all");
    assert.equal(req1.request.query.limit, "20");
    p.answer(req1.index, { items: rows("first", 3), has_more: true, next_cursor: "cursor1" });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 3);
    assert.match(await p.page.locator("#body-history .hist-item").first().innerText(), /first message 0/);

    await p.page.click('#body-history button:has-text("Load more")');
    const req2 = await p.waitForRequest((r) => r.query.cursor === "cursor1");
    p.answer(req2.index, { items: rows("second", 1), has_more: false });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 4);
    assert.equal(await p.page.locator('#body-history button:has-text("Load more")').count(), 0);

    await p.page.fill("#hist-search", "weather");
    await p.page.click('#body-history button:has-text("Find")');
    const req3 = await p.waitForRequest((r) => r.query.q === "weather");
    p.answer(req3.index, { items: [{ id: "w1", speaker: "Someone in person", channel: "in_person", text: "How is the weather?", ts_ms: 1760000001000 }] });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 1);
    assert.match(await p.page.locator("#body-history .hist-item").innerText(), /How is the weather\?/);
    assert.match(await p.page.locator("#body-history .hist-item").innerText(), /In person/);

    assert.equal(await p.page.locator('#body-history .pill-btn:has-text("Intercom")').count(), 0);
    await p.page.click('#body-history .pill-btn:has-text("My Delta Chat")');
    const req4 = await p.waitForRequest((r) => r.query.channel === "deltachat");
    p.answer(req4.index, { items: [] });
    await p.page.waitForFunction(() => document.querySelector("#body-history").textContent.includes("No matching messages"));
    await p.shot("history");
  } finally { await p.close(); }
});

test("regression: history survives state pushes, tab switches, and stale responses", async () => {
  const p = await open();
  try {
    await p.tab("history");
    const req1 = await p.waitForRequest((r) => r.path === "/remote/api/history");
    p.answer(req1.index, { items: rows("page1", 2), has_more: true, next_cursor: "page2" });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 2);
    await p.page.click('#body-history button:has-text("Load more")');
    const req2 = await p.waitForRequest((r) => r.query.cursor === "page2");
    p.answer(req2.index, { items: rows("page2", 1), has_more: false });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 3);

    p.deliver(stateUpdate({}, { live_state: "speaking" }));
    p.deliver(stateUpdate({}, { voice: "bm_fable" }));
    await p.page.waitForTimeout(100);
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);
    await p.tab("status");
    await p.tab("history");
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 3);

    // A stale search response must not replace the current list.
    await p.page.fill("#hist-search", "next");
    await p.page.click('#body-history button:has-text("Find")');
    const stale = await p.waitForRequest((r) => r.query.q === "next");
    await p.page.fill("#hist-search", "newer");
    await p.page.click('#body-history button:has-text("Find")');
    const fresh = await p.waitForRequest((r) => r.query.q === "newer");
    p.answer(fresh.index, { items: rows("fresh", 1) });
    await p.page.waitForFunction(() => document.querySelectorAll("#body-history .hist-item").length === 1);
    p.answer(stale.index, { items: [] });
    await p.page.waitForTimeout(100);
    assert.equal(await p.page.locator("#body-history .hist-item").count(), 1);
    assert.doesNotMatch(await p.page.locator("#body-history").innerText(), /No matching messages/);
    await p.shot("history-regression");
  } finally { await p.close(); }
});

test("regression: voice selection updates from the response without a state push", async () => {
  const p = await open();
  try {
    await p.tab("settings");
    const pressed = () => p.page.locator(".voice-btn[aria-pressed=true]").allInnerTexts();
    assert.deepEqual(await pressed(), ["Heart\nActive"]);
    await p.page.click('.voice-btn:has-text("Fable")');
    const req = await p.waitForRequest((r) => r.path === "/remote/api/voice");
    assert.deepEqual(req.request.body, { voice: "bm_fable" });
    p.answer(req.index, { ok: true, message: "Voice updated.", state: stateUpdate({}, { voice: "bm_fable" }) });
    await p.page.waitForFunction(() => document.querySelector('.voice-btn[aria-pressed="true"]').textContent.startsWith("Fable"));
    assert.deepEqual(await pressed(), ["Fable\nActive"]);
    assert.equal(await p.page.locator(".voice-btn .row-sub").count(), 1);
    await p.shot("settings");
  } finally { await p.close(); }
});

test("Diagnostics records a capture, streams progress, and manages the file", async () => {
  const p = await open();
  const name = "capture-greg-20261123-120000";
  try {
    await p.tab("diagnostics");
    const list = await p.waitForRequest((r) => r.path === "/remote/api/captures" && r.method === "GET");
    assert.equal(list.request.query.token, undefined);
    assert.equal(list.request.headers["x-lark-remote-token"], TOKEN);
    p.answer(list.index, { captures: [] });
    await p.page.waitForFunction(() => document.querySelector("#capture-list .empty"));
    assert.equal(await p.page.textContent('#body-diagnostics .pill-btn[aria-pressed="true"]'), "60 s");
    assert.match(await p.page.textContent(".capture-privacy"), /Room audio stays on this server/);

    await p.page.click('#body-diagnostics .pill-btn:has-text("15 s")');
    await p.page.click("#capture-start");
    const start = await p.waitForRequest((r) => r.path === "/remote/api/capture");
    assert.deepEqual(start.request.body, { seconds: 15 });
    assert.equal(start.request.query.token, undefined);
    assert.equal(start.request.headers["x-lark-remote-token"], TOKEN);
    assert.equal(await p.page.getAttribute("#capture-start", "aria-busy"), "true");
    p.answer(start.index, { id: name, name, seconds: 15, started_at: "2026-11-23T12:00:00Z" });
    await p.page.waitForFunction(() => document.querySelector("#capture-start").disabled);

    p.deliverCapture({ state: "recording", id: name, name, seconds: 15, remaining: 15, started_at: "2026-11-23T12:00:00Z" });
    await p.page.waitForFunction(() => document.querySelector("#capture-progress").textContent.includes("15 s left"));
    assert.match(await p.page.textContent("#capture-reason"), /capture is in progress/i);
    p.deliverCapture({ state: "recording", id: name, name, seconds: 15, remaining: 8, started_at: "2026-11-23T12:00:00Z" });
    await p.page.waitForFunction(() => document.querySelector("#capture-progress").textContent.includes("8 s left"));

    p.deliverCapture({ state: "done", id: name, name, ok: true });
    const reload = await p.waitForRequest((r) => r.path === "/remote/api/captures" && r.method === "GET");
    p.answer(reload.index, { captures: [{ name, seconds: 15, size: 480044, created: "2026-11-23T12:00:00Z", computing: false }] });
    await p.page.waitForFunction((n) => {
      const rows = document.querySelectorAll("#capture-list article.capture-row");
      return rows.length === 1 && document.querySelector("#capture-list").textContent.includes(n);
    }, name);
    assert.equal(await p.page.locator("#capture-start").isDisabled(), false);

    await p.page.click('#capture-list button:has-text("Play")');
    const wav = await p.waitForPath((r) => r.path === "/remote/api/captures/" + name + ".wav");
    assert.equal(wav.query.token, TOKEN);

    await p.page.click('#capture-list button:has-text("Transcribe")');
    const transcribe = await p.waitForRequest((r) => r.path === "/remote/api/captures/" + name + "/transcribe");
    assert.equal(transcribe.request.method, "POST");
    assert.equal(transcribe.request.query.token, undefined);
    assert.equal(transcribe.request.headers["x-lark-remote-token"], TOKEN);
    p.answer(transcribe.index, { name, transcript: "a tiny hello", stt_ms: 40, audio_ms: 15000, engine: "parakeet" });
    await p.page.waitForFunction(() => document.querySelector("#capture-list details.capture-transcript p").textContent === "a tiny hello");

    await p.page.click('#capture-list button:has-text("Delete")');
    const del = await p.waitForRequest((r) => r.path === "/remote/api/captures/" + name);
    assert.equal(del.request.method, "DELETE");
    assert.equal(del.request.query.token, undefined);
    assert.equal(del.request.headers["x-lark-remote-token"], TOKEN);
    p.answer(del.index, { ok: true });
    await p.page.waitForFunction(() => document.querySelectorAll("#capture-list article.capture-row").length === 0);
    await p.shot("diagnostics");
  } finally { await p.close(); }
});

test("capture stays disabled during a voice turn with the reason shown", async () => {
  const p = await open();
  try {
    await p.tab("diagnostics");
    const list = await p.waitForRequest((r) => r.path === "/remote/api/captures" && r.method === "GET");
    p.answer(list.index, { captures: [] });
    p.deliver(stateUpdate({}, { live_state: "speaking" }));
    await p.page.waitForFunction(() => document.querySelector("#capture-start").disabled);
    assert.match(await p.page.textContent("#capture-reason"), /speaking/i);
    p.deliver(stateUpdate({}, { live_state: "idle" }));
    await p.page.waitForFunction(() => !document.querySelector("#capture-start").disabled);
    assert.equal(await p.page.textContent("#capture-reason"), "Ready to record.");
  } finally { await p.close(); }
});

test("tabs support keyboard navigation and controls are labelled", async () => {
  const p = await open();
  try {
    assert.equal(await p.page.getAttribute('nav[role="tablist"]', "aria-label"), "Remote sections");
    assert.equal(await p.page.locator('.tab-btn[role="tab"]').count(), 5);
    assert.equal(await p.page.getAttribute("#tab-stories", "aria-controls"), "panel-stories");
    assert.equal(await p.page.getAttribute("#panel-stories", "aria-labelledby"), "tab-stories");
    assert.equal(await p.page.getAttribute("#live-state", "aria-live"), "polite");
    await p.page.focus("#tab-status");
    await p.page.keyboard.press("ArrowRight");
    assert.equal(await p.page.getAttribute("#tab-stories", "aria-selected"), "true");
    assert.equal(await p.page.evaluate(() => document.activeElement.id), "tab-stories");
    await p.page.keyboard.press("End");
    assert.equal(await p.page.getAttribute("#tab-settings", "aria-selected"), "true");
    await p.page.keyboard.press("Home");
    assert.equal(await p.page.getAttribute("#tab-status", "aria-selected"), "true");
    await p.page.focus("#tab-status");
    await p.page.keyboard.press("Tab");
    assert.equal(await p.page.evaluate(() => document.activeElement.id), "say-input");
    await p.tab("history");
    assert.equal(await p.page.locator('label[for="hist-search"]').count(), 1);
    assert.equal(await p.page.getAttribute('#body-history .pill-group', "aria-label"), "Filter by channel");
    await p.tab("settings");
    assert.equal(await p.page.getAttribute("#body-settings .voice-grid", "aria-label"), "Voice selection");
  } finally { await p.close(); }
});

test("state pushes keep in-progress input and pending controls", async () => {
  const p = await open();
  try {
    await p.page.fill("#say-input", "half typed");
    await p.page.focus("#say-input");
    p.deliver(stateUpdate({}, { live_state: "thinking" }));
    await p.page.waitForFunction(() => document.querySelector("#live-state-text").textContent === "Thinking");
    assert.equal(await p.page.inputValue("#say-input"), "half typed");
    assert.equal(await p.page.evaluate(() => document.activeElement.id), "say-input");

    await p.page.click('#body-status button[type="submit"]');
    const say = await p.waitForRequest((r) => r.path === "/remote/api/say");
    assert.deepEqual(say.request.body, { text: "half typed" });
    await p.page.evaluate(() => { document.querySelector('#body-status button[type="submit"]').dataset.marker = "kept"; });
    p.deliver(stateUpdate({}, { live_state: "speaking" }));
    await p.page.waitForFunction(() => document.querySelector("#live-state-text").textContent === "Speaking");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "data-marker"), "kept");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), "true");
    p.answer(say.index, { ok: true, message: "Said it.", state: stateUpdate() });
    await p.page.waitForFunction(() => document.querySelector("#feedback").textContent === "Said it.");
    await p.page.waitForTimeout(50);
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "data-marker"), "kept");
    assert.equal(await p.page.getAttribute('#body-status button[type="submit"]', "aria-busy"), null);
  } finally { await p.close(); }
});

test("inactive panels stay findable instead of display:none", async () => {
  const p = await open();
  try {
    const inactive = await p.page.evaluate(() => {
      const panel = document.getElementById("panel-stories");
      const style = getComputedStyle(panel);
      return { hidden: panel.getAttribute("hidden"), display: style.display, contentVisibility: style.contentVisibility };
    });
    assert.equal(inactive.hidden, "until-found");
    assert.notEqual(inactive.display, "none");
    assert.equal(inactive.contentVisibility, "hidden");
    await p.tab("stories");
    assert.equal(await p.page.evaluate(() => getComputedStyle(document.getElementById("panel-stories")).contentVisibility), "visible");
  } finally { await p.close(); }
});
