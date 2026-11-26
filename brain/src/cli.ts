// Small operator CLI. Every command talks to the running brain over the same
// loopback HTTP API the remote page uses; it never opens the database itself.
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

interface Json {
  [key: string]: unknown;
}

const TIMEOUT_MS = 15_000;

function dataDir(): string {
  return process.env.LARK_DATA_DIR ?? join(homedir(), ".local", "share", "lark");
}

export function envFileToken(): string {
  const config = process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config");
  let text: string;
  try {
    text = readFileSync(join(config, "lark", "lark.env"), "utf8");
  } catch {
    return "";
  }
  for (const line of text.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("LARK_TOKEN=")) continue;
    let value = trimmed.slice("LARK_TOKEN=".length).trim();
    if (value.length >= 2 && (value[0] === '"' || value[0] === "'") && value.at(-1) === value[0]) {
      value = value.slice(1, -1);
    }
    return value;
  }
  return "";
}

export function token(): string {
  if (process.env.LARK_TOKEN !== undefined && process.env.LARK_TOKEN !== "") return process.env.LARK_TOKEN;
  const path = join(dataDir(), "token");
  try {
    const stored = readFileSync(path, "utf8").trim();
    if (stored !== "") return stored;
  } catch {
    // Fall back to the unit's environment file so the CLI works after setup.
  }
  return envFileToken();
}

function baseUrl(): string {
  if (process.env.LARK_URL !== undefined && process.env.LARK_URL !== "") return process.env.LARK_URL.replace(/\/+$/, "");
  const host = process.env.LARK_HOST ?? "127.0.0.1";
  const port = process.env.LARK_PORT ?? "8765";
  return `http://${host}:${port}`;
}

async function request(path: string, init: RequestInit = {}, timeoutMs = TIMEOUT_MS): Promise<Json> {
  const headers = new Headers(init.headers);
  const admin = token();
  if (admin !== "") headers.set("X-Lark-Token", admin);
  if (init.body !== undefined) headers.set("content-type", "application/json");
  const response = await fetch(`${baseUrl()}${path}`, {
    ...init,
    headers,
    signal: AbortSignal.timeout(timeoutMs),
  });
  const text = await response.text();
  let parsed: Json = {};
  if (text !== "") {
    try {
      parsed = JSON.parse(text) as Json;
    } catch {
      if (!response.ok) throw new Error(`HTTP ${response.status}: ${text.trim()}`);
      throw new Error("server returned invalid JSON");
    }
  }
  if (!response.ok) {
    const message = typeof parsed.error === "string" ? parsed.error : `HTTP ${response.status}`;
    throw new Error(message);
  }
  return parsed;
}

function list(value: unknown): Json[] {
  return Array.isArray(value) ? value.filter((item): item is Json => item !== null && typeof item === "object") : [];
}

function str(value: unknown, fallback = ""): string {
  return typeof value === "string" ? value : typeof value === "number" ? String(value) : fallback;
}

function num(value: unknown): number {
  return typeof value === "number" ? value : 0;
}

function humanBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(1)} ${units[unit]}`;
}

async function status(): Promise<void> {
  const [health, companions, terminals, library] = await Promise.all([
    request("/health"),
    request("/api/companions"),
    request("/api/terminals"),
    request("/api/library"),
  ]);
  process.stdout.write(
    `Server: ok=${String(health.ok)} opencode=${String(health.opencode)} stt=${String(health.stt)} tts=${String(health.tts)} deltachat=${String(health.deltachat)}\n`,
  );
  const models = health.models;
  if (models !== null && typeof models === "object") {
    const parts: string[] = [];
    for (const name of ["parakeet", "kokoro", "smart_turn", "silero"]) {
      const model = (models as Record<string, unknown>)[name];
      if (model === null || typeof model !== "object") continue;
      const state = str((model as Json).state, "unknown");
      parts.push(`${name}=${state}`);
    }
    if (parts.length > 0) process.stdout.write(`Models: ${parts.join(" ")}\n`);
  }
  process.stdout.write(`Library (${str(library.dir)}):\n`);
  const packs = list(library.packs);
  for (const pack of packs) {
    process.stdout.write(`  ${str(pack.name)} ${str(pack.state)}${pack.installed_bytes === undefined ? "" : ` ${humanBytes(num(pack.installed_bytes))}`}\n`);
  }
  const companionsList = list(companions.companions);
  process.stdout.write(`Companions (${companionsList.length}):\n`);
  for (const companion of companionsList) printCompanion(companion);
  const terminalList = list(terminals.terminals);
  process.stdout.write(`Terminals (${terminalList.length}):\n`);
  for (const terminal of terminalList) printTerminal(terminal);
}

function printCompanion(companion: Json): void {
  const dc = companion.dc_account_id === 0 || companion.dc_account_id === undefined ? "" : `\tdc=${str(companion.dc_account_id)}`;
  process.stdout.write(
    `${str(companion.id)}\t${str(companion.name)}\tonline=${String(companion.online)}\tdormant=${String(companion.dormant)}${dc}\n`,
  );
}

function printTerminal(terminal: Json): void {
  process.stdout.write(
    `${str(terminal.id)}\tcompanion=${str(terminal.companion_id)}\tonline=${String(terminal.online)}\tip=${str(terminal.ip)}\tfw=${str(terminal.firmware)}\n`,
  );
}

async function doctor(): Promise<number> {
  const problems: string[] = [];
  const socket = process.env.LARK_EDGE_SOCK ?? join(dataDir(), "edge.sock");
  if (!existsSync(socket)) problems.push(`edge socket missing: ${socket}`);
  try {
    const health = await request("/health", {}, 5000);
    if (health.ok !== true) problems.push("/health did not report ok");
    const models = health.models;
    if (models !== null && typeof models === "object") {
      for (const [name, value] of Object.entries(models as Record<string, unknown>)) {
        if (value === null || typeof value !== "object") continue;
        const state = str((value as Json).state);
        if (state !== "ready" && state !== "disabled") problems.push(`model ${name}=${state}`);
      }
    }
    if (health.deltachat === false) problems.push("Delta Chat is not ready");
    const dc = await request("/api/diagnostics", {}, 5000);
    const dcInfo = dc.deltachat;
    if (dcInfo !== null && typeof dcInfo === "object") {
      const orphans = Array.isArray((dcInfo as Json).orphans) ? (dcInfo as Json).orphans as unknown[] : [];
      if (orphans.length > 0) problems.push(`orphaned Delta Chat accounts: ${orphans.join(",")}`);
    }
  } catch (error) {
    problems.push(`HTTP: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (problems.length === 0) {
    process.stdout.write("Doctor: all checks passed\n");
    return 0;
  }
  process.stdout.write(`Doctor: ${problems.length} problem${problems.length === 1 ? "" : "s"}\n`);
  for (const problem of problems) process.stdout.write(`  - ${problem}\n`);
  return 1;
}

async function companions(args: string[]): Promise<void> {
  if (args.length === 0) {
    const response = await request("/api/companions");
    for (const companion of list(response.companions)) printCompanion(companion);
    return;
  }
  if (args.length === 2 && args[0] === "rm") {
    await request(`/api/companions/${encodeURIComponent(args[1] ?? "")}`, { method: "DELETE" });
    process.stdout.write(`Removed companion ${args[1]}\n`);
    return;
  }
  if (args.length === 3 && args[0] === "add") {
    const response = await request("/api/companions", {
      method: "POST",
      body: JSON.stringify({ name: args[1], form: args[2] }),
    });
    process.stdout.write(`Added companion ${str(response.id)} (${str(response.name)})\n`);
    return;
  }
  throw new Error("Usage: lark companions [add <name> <form> | rm <id>]");
}

async function terminals(args: string[]): Promise<void> {
  if (args.length === 0) {
    const response = await request("/api/terminals");
    for (const terminal of list(response.terminals)) printTerminal(terminal);
    return;
  }
  if (args.length === 3 && args[0] === "attach") {
    const response = await request(`/api/terminals/${encodeURIComponent(args[1] ?? "")}/attach`, {
      method: "POST",
      body: JSON.stringify({ companion_id: args[2] }),
    });
    process.stdout.write(`Attached ${str(response.id)} to ${str(response.companion_id)}\n`);
    return;
  }
  if (args.length === 2 && args[0] === "detach") {
    const response = await request(`/api/terminals/${encodeURIComponent(args[1] ?? "")}/detach`, { method: "POST", body: "{}" });
    process.stdout.write(`Detached ${str(response.id)}\n`);
    return;
  }
  if (args.length === 2 && args[0] === "rm") {
    await request(`/api/terminals/${encodeURIComponent(args[1] ?? "")}`, { method: "DELETE" });
    process.stdout.write(`Removed terminal ${args[1]}\n`);
    return;
  }
  throw new Error("Usage: lark terminals [attach <terminal> <companion> | detach <terminal> | rm <terminal>]");
}

async function tunnels(args: string[]): Promise<void> {
  if (args.length === 0) {
    const response = await request("/api/remote/tunnels");
    for (const tunnel of list(response.tunnels)) {
      process.stdout.write(`${str(tunnel.companion_id)}\tactive=${String(tunnel.active)}\tcontacts=${str(tunnel.contacts)}\t${str(tunnel.url)}\n`);
    }
    return;
  }
  if (args.length === 2 && args[0] === "stop") {
    await request(`/api/remote/tunnels/${encodeURIComponent(args[1] ?? "")}`, { method: "DELETE" });
    process.stdout.write(`Stopped remote controls for ${args[1]}\n`);
    return;
  }
  throw new Error("Usage: lark tunnels [stop <companion>]");
}

async function capture(args: string[]): Promise<void> {
  if (args.length < 1 || args.length > 2) throw new Error("Usage: lark capture <companion> [seconds]");
  const seconds = args.length === 2 ? Number.parseInt(args[1] ?? "", 10) : 15;
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 60) throw new Error("seconds must be 1..60");
  process.stderr.write(`Recording ${seconds}s of microphone audio; stay quiet or speak normally.\n`);
  const response = await request(
    "/api/capture",
    { method: "POST", body: JSON.stringify({ companion_id: args[0], seconds }) },
    (seconds + 30) * 1000,
  );
  process.stdout.write(`${str(response.path)}\n`);
}

async function library(args: string[]): Promise<void> {
  if (args.length > 1) throw new Error("Usage: lark library [--plan|--status]");
  if (args.length === 1 && args[0] !== "--plan" && args[0] !== "--status") {
    throw new Error("Usage: lark library [--plan|--status]");
  }
  const plan = await request("/api/library");
  process.stdout.write(`${JSON.stringify(plan, null, 2)}\n`);
}

export async function run(argv: string[]): Promise<number> {
  const [command, ...args] = argv;
  try {
    switch (command) {
      case "status":
        await status();
        return 0;
      case "doctor":
        return await doctor();
      case "companions":
        await companions(args);
        return 0;
      case "terminals":
        await terminals(args);
        return 0;
      case "tunnels":
        await tunnels(args);
        return 0;
      case "capture":
        await capture(args);
        return 0;
      case "library":
        await library(args);
        return 0;
      case undefined:
      case "help":
      case "--help":
        process.stdout.write(
          "Usage: lark <status|doctor|companions|terminals|tunnels|capture|library|ota|deploy> [args]\n",
        );
        return command === undefined ? 1 : 0;
      default:
        throw new Error(`unknown command ${JSON.stringify(command)}`);
    }
  } catch (error) {
    process.stderr.write(`lark: ${error instanceof Error ? error.message : String(error)}\n`);
    return 1;
  }
}

if (process.argv[1] !== undefined && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await run(process.argv.slice(2));
}
