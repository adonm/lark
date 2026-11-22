import {api} from "./api.js";
import {$, node, say, svgNS} from "./dom.js";
import {state} from "./state.js";

let diagnosticsOpen = false, diagnosticsTimer, diagnosticsRequest;

function pauseDiagnostics() {
  clearTimeout(diagnosticsTimer);
  diagnosticsRequest?.abort();
  diagnosticsRequest = null;
}

function setDiagnostics(open) {
  pauseDiagnostics();
  diagnosticsOpen = open && !!state.app && !state.app.scope;
  $("diagnostics").hidden = !diagnosticsOpen;
  $("workspace").hidden = !state.app || diagnosticsOpen;
  $("sidebar-toggle").hidden = !state.app || !!state.app.scope || diagnosticsOpen;
  $("diagnostics-toggle").textContent = diagnosticsOpen ? "Back to Lark" : "Diagnostics";
  $("diagnostics-toggle").setAttribute("aria-expanded", String(diagnosticsOpen));
  if (!diagnosticsOpen) return;
  say("diag-status", "Connecting…");
  $("diag-heading").focus();
  pollDiagnostics();
}

async function pollDiagnostics() {
  const current = state.app;
  if (!diagnosticsOpen || document.hidden || diagnosticsRequest || !current || current.scope) return;
  const request = diagnosticsRequest = new AbortController();
  const start = performance.now();
  try {
    const data = await api(current, "/api/diagnostics", {signal:request.signal});
    if (diagnosticsRequest !== request) return;
    renderDiagnostics(data);
    say("diag-status", `Live · ${new Date(data.generated_at).toLocaleTimeString()} · updates every 2 s`);
  } catch {
    if (diagnosticsRequest === request) say("diag-status", "Telemetry unavailable · retrying…");
  } finally {
    if (diagnosticsRequest === request) {
      diagnosticsRequest = null;
      diagnosticsTimer = setTimeout(pollDiagnostics, Math.max(0, 2000 - (performance.now() - start)));
    }
  }
}

$("diagnostics-toggle").onclick = () => setDiagnostics(!diagnosticsOpen);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) pauseDiagnostics();
  else pollDiagnostics();
});

const diagFormat = (v, unit = "", digits = 0) => Number.isFinite(v) ? `${v.toFixed(digits)}${unit}` : "—";
const diagBytes = (v, base = 1024, unit = " KiB") => Number.isFinite(v) ? diagFormat(v / base, unit, 1) : "—";
const diagMB = v => diagBytes(v, 1048576, " MiB");
const diagGB = v => diagBytes(v, 1073741824, " GiB");
const diagMS = v => diagFormat(v, " ms", 1);
const diagPct = v => diagFormat(v, "%", 1);
function diagUptime(v) {
  if (!Number.isFinite(v)) return "—";
  if (v < 60) return `${v}s`;
  const d = Math.floor(v / 86400), h = Math.floor(v / 3600) % 24, m = Math.floor(v / 60) % 60;
  return [d && `${d}d`, (d || h) && `${h}h`, `${m}m`].filter(Boolean).join(" ");
}

function diagTile(title, fields) {
  const card = node("article", "card diag-tile");
  card.append(node("h4", "", title));
  const list = node("dl", "diag-fields");
  for (const [key, value] of fields) list.append(node("dt", "", key), node("dd", "", value));
  card.append(list);
  return card;
}

function diagBar(card, title, pct) {
  if (!Number.isFinite(pct)) return;
  const meter = node("progress", "diag-bar");
  meter.max = 100;
  meter.value = Math.min(100, Math.max(0, pct));
  meter.setAttribute("aria-label", `${title}: ${diagPct(pct)}`);
  card.append(meter);
}

function diagSparkline(samples) {
  const chart = document.createElementNS(svgNS, "svg");
  chart.setAttribute("viewBox", "0 0 120 28");
  chart.setAttribute("class", "diag-spark");
  chart.setAttribute("aria-hidden", "true");
  const points = samples.filter(Number.isFinite);
  if (points.length) {
    const path = document.createElementNS(svgNS, "polyline");
    const max = Math.max(1, ...points);
    const coords = points.map((v, i) => `${2+i*116/Math.max(1, points.length-1)},${25-v*22/max}`);
    if (coords.length === 1) coords.push(`118,${coords[0].split(",")[1]}`);
    path.setAttribute("points", coords.join(" "));
    chart.append(path);
  }
  return chart;
}

function renderDiagnostics(data) {
  const {bridge, tts, host, services, latency, recent_turns:turns, terminals} = data;
  const usage = Number.isFinite(host.mem_used_bytes) && host.mem_total_bytes > 0 ? host.mem_used_bytes / host.mem_total_bytes * 100 : null;
  const system = [
    diagTile("Lark server", [
      ["CPU / RSS", `${diagPct(bridge.cpu_pct)} / ${diagMB(bridge.rss_bytes)}`],
      ["Heap alloc / in use", `${diagMB(bridge.heap_alloc_bytes)} / ${diagMB(bridge.heap_inuse_bytes)}`],
      ["Goroutines", diagFormat(bridge.goroutines)], ["Uptime", diagUptime(bridge.uptime_s)],
      ["SQLite DB + WAL", diagBytes(bridge.db_bytes)],
      ["Last backup", bridge.last_backup ? new Date(bridge.last_backup).toLocaleString() : "—"],
    ]),
    diagTile("Kokoro TTS", [
      ["Engine", tts.engine || "—"],
      ["CPU / RSS", "Included in Lark server"],
      ["State", tts.ready ? "Ready" : "Not ready"],
    ]),
    diagTile("Linux host", [
      ["CPU / 1m load", `${diagPct(host.cpu_pct)} / ${diagFormat(host.load_1m, "", 2)}`],
      ["RAM", `${diagGB(host.mem_used_bytes)} / ${diagGB(host.mem_total_bytes)}`],
    ]),
  ];
  diagBar(system[2], "Host RAM used", usage);
  $("diag-system").replaceChildren(...system);
  $("diag-services").replaceChildren(...Object.entries({stt:"Parakeet Vulkan", opencode:"OpenCode", tts:"Kokoro", opentunnel:"OpenTunnel"}).map(([key, label]) => {
    const service = node("li", "badge", `${label} · ${services[key] ? (key === "opentunnel" ? "installed" : "ready") : "unavailable"}`);
    service.dataset.variant = services[key] ? "success" : "warning";
    return service;
  }));
  $("diag-latency").replaceChildren(...Object.entries({speech_end_to_commit_ms:"Speech End → Commit",
    stt_ms:"Speech to Text", gate_ms:"OpenCode Gate",
    llm_first_ms:"OpenCode TTFT", first_sentence_ms:"First Sentence", llm_total_ms:"OpenCode Total",
    tts_queue_ms:"TTS Queue", tts_infer_ms:"TTS Inference", tts_first_ms:"Kokoro TTS", first_pcm_ms:"First PCM",
    e2e_ms:"Voice E2E", render_ms:"SVG→RLE565 Render"}).map(([key, title]) => {
    const m = latency[key] || {samples:[]};
    const card = diagTile(title, [["Last", diagMS(m.last)], ["p50 / p95", `${diagMS(m.p50)} / ${diagMS(m.p95)}`]]);
    card.append(diagSparkline(m.samples));
    if (key === "render_ms") {
      card.append(node("p", "small", `Last frame ${diagBytes(latency.frame_bytes.last)} / ${diagBytes(latency.frame_raw_bytes.last)} raw`));
    }
    return card;
  }));
  const rows = turns.map(t => {
    const row = node("tr");
    row.append(...[new Date(t.ts_ms).toLocaleTimeString(), `${t.character} / ${t.terminal}`, t.source, ...[
      t.stt_ms, t.llm_first_ms, t.llm_total_ms, t.tts_first_ms, t.e2e_ms,
    ].map(diagMS)].map(value => node("td", "", value)));
    return row;
  });
  if (!rows.length) {
    const row = node("tr"), cell = node("td", "", "No turns yet.");
    cell.colSpan = 8;
    row.append(cell);
    rows.push(row);
  }
  $("diag-turns").replaceChildren(...rows);
  $("diag-devices").replaceChildren(...(terminals.length ? terminals.map(d => {
    const f = d.last_frame;
    const battery = d.has_battery === false ? "USB" : Number.isFinite(d.battery_pct) ?
      `${diagFormat(d.battery_pct, "%")} · ${diagFormat(d.battery_mv, " mV")}${d.charging ? " · charging" : ""}` : "—";
    const card = diagTile(d.name || d.terminal_id, [
      ["Terminal", d.terminal_id], ["Companion", d.companion_id || "—"], ["Board / FW", `${d.board || d.kind} / ${d.firmware || "—"}`],
      ["OTA", d.kind === "web" ? "—" : `${d.ota || "—"} · ${d.ota_bundle ? "image on server" : "no image"}`],
      ["WS / RTT", `${d.ws_connected ? `Connected · ${d.mode}` : "Disconnected"} / ${diagMS(d.rtt_ms)}`],
      ["Wi-Fi", diagFormat(d.wifi_rssi, " dBm")], ["ESP32 CPU", diagPct(d.cpu_pct)],
      ["SRAM free / min", `${diagBytes(d.free_heap)} / ${diagBytes(d.min_free_heap)}`],
      ["PSRAM free", diagMB(d.free_psram)], ["Uptime", diagUptime(d.uptime_s)], ["Battery", battery],
      ["Ambient RMS", diagFormat(d.ambient_rms, "", 1)], ["Volume", Number.isFinite(d.volume) ? `${d.volume}% · ${d.auto_volume ? "Auto" : "Manual"}` : "—"],
      ["Last frame", f ? `${diagBytes(f.frame_bytes)} · ${diagMS(f.render_ms)} · ${f.frame_codec}` : "—"],
      ...(d.last_frame_rev != null ? [["LCD revision / flush", `${d.last_frame_rev} / ${diagMS(d.display_flush_ms)}`]] : []),
      ...(d.input_drops != null ? [["Input drops", diagFormat(d.input_drops, "")]] : []),
      ...(d.audio_drops != null ? [["PCM underruns / drops", `${diagFormat(d.audio_underruns, "")} / ${diagFormat(d.audio_drops, "")}`]] : []),
    ]);
    diagBar(card, "ESP32 CPU", d.cpu_pct);
    return card;
  }) : [node("p", "small", "No terminals have checked in.")]));
}

export {setDiagnostics};
