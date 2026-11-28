// Process entry point: compose the typed brain, edge client, Delta Chat,
// portraits, remote controls and HTTP server, then shut every child and socket
// down in reverse order. Configuration is environment-only so systemd can own
// secrets and paths.
import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { LarkDb } from "./db.ts";
import { EdgeClient } from "./edge_client.ts";
import { Brain } from "./opencode.ts";
import { Library } from "./library.ts";
import { TimerScheduler, type Timer } from "./timers.ts";
import { DcService } from "./deltachat.ts";
import { PortraitManager } from "./portraits.ts";
import { CaptureStore } from "./capture.ts";
import { RemoteManager, resolveTunnelBin } from "./remote.ts";
import { Router } from "./router.ts";
import { startHttp, type RunningHttp } from "./http.ts";
import { identifySong } from "./song_id.ts";
import type { AudioOps } from "./tools.ts";
import { errMessage } from "./util.ts";

export interface RuntimeConfig {
  directory: string;
  dataDir: string;
  dbPath: string;
  adminToken: string;
  host: string;
  port: number;
  edgeSock: string;
  relay: string;
  dcBin: string | undefined;
  disableDc: boolean;
  llmBaseURL: string | undefined;
  llmModel: string;
  agentFile: string;
  libraryDir: string;
  tunnelBin: string | undefined;
  opencodeDataDir: string;
}

export interface RuntimeOptions {
  config: RuntimeConfig;
  log?: (line: string) => void;
}

export interface Runtime {
  config: RuntimeConfig;
  db: LarkDb;
  edge: EdgeClient;
  brain: Brain;
  router: Router;
  dc: DcService | undefined;
  portraits: PortraitManager;
  remote: RemoteManager;
  captures: CaptureStore;
  http: RunningHttp;
  close(): Promise<void>;
}

function env(name: string, fallback = ""): string {
  const value = process.env[name];
  return value === undefined ? fallback : value;
}

function bool(name: string): boolean {
  return /^(1|true|yes|on)$/i.test(env(name));
}

export function defaultDataDir(): string {
  return env("LARK_DATA_DIR", join(homedir(), ".local", "share", "lark"));
}

function loadToken(dataDir: string): string {
  const fromEnv = env("LARK_TOKEN");
  if (fromEnv !== "") return fromEnv;
  const path = join(dataDir, "token");
  try {
    const stored = readFileSync(path, "utf8").trim();
    if (stored !== "") return stored;
  } catch {
    // First start creates a token below.
  }
  const token = randomBytes(24).toString("hex");
  writeFileSync(path, `${token}\n`, { mode: 0o600 });
  return token;
}

export function configFromEnv(): RuntimeConfig {
  const dataDir = resolve(defaultDataDir());
  mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  const directory = resolve(env("LARK_DIRECTORY", process.cwd()));
  const port = Number.parseInt(env("LARK_PORT", "8765"), 10);
  const dcBin = env("LARK_DC_RPC_BIN") || (existsSync("/usr/local/bin/deltachat-rpc-server") ? "/usr/local/bin/deltachat-rpc-server" : "deltachat-rpc-server");
  const tunnelBin = resolveTunnelBin();
  return {
    directory,
    dataDir,
    dbPath: env("LARK_DB_PATH", join(dataDir, "lark.db")),
    adminToken: loadToken(dataDir),
    host: env("LARK_HOST", "127.0.0.1"),
    port: Number.isInteger(port) && port >= 0 ? port : 8765,
    edgeSock: env("LARK_EDGE_SOCK", join(dataDir, "edge.sock")),
    relay: env("LARK_CHATMAIL_RELAY", "nine.testrun.org"),
    dcBin: bool("LARK_DISABLE_DC") ? undefined : dcBin,
    disableDc: bool("LARK_DISABLE_DC"),
    llmBaseURL: env("LARK_LLM_BASE_URL") || undefined,
    llmModel: env("LARK_LLM_MODEL", "mock"),
    agentFile: env("LARK_AGENT_FILE", resolve(import.meta.dirname, "../../agents/lark.md")),
    libraryDir: env("LARK_LIBRARY_DIR", join(dataDir, "library")),
    tunnelBin: env("LARK_DISABLE_TUNNELS") === "1" ? undefined : tunnelBin,
    opencodeDataDir: env("LARK_OPENCODE_DIR", join(dataDir, "opencode")),
  };
}

export async function startRuntime(options: RuntimeOptions): Promise<Runtime> {
  const config = options.config;
  const log = options.log ?? ((line: string) => process.stderr.write(`lark-brain: ${line}\n`));
  mkdirSync(config.dataDir, { recursive: true, mode: 0o700 });
  mkdirSync(config.opencodeDataDir, { recursive: true, mode: 0o700 });
  const db = new LarkDb(config.dbPath);
  const library = new Library(config.libraryDir);
  let router: Router | undefined;

  const timers = new TimerScheduler(db, {
    onFire: (timer: Timer) => {
      if (router === undefined) return;
      const terminal = router.terminalForCompanion(timer.companionId);
      if (terminal === undefined) return;
      if (timer.chime !== "") {
        options.log?.(`timer ${timer.id} chime ${timer.chime}`);
      }
      if (timer.wakeAgent) {
        void router.wakeTimer(timer);
        return;
      }
      void router.speakPhrase(terminal, timer.say === "" ? timer.label : timer.say);
    },
  });

  const provider =
    config.llmBaseURL === undefined
      ? undefined
      : { id: "larkmock", baseURL: config.llmBaseURL, models: [config.llmModel] };
  const modelSpec = provider === undefined ? undefined : `larkmock/${config.llmModel}`;
  const audio: AudioOps = {
    metronome: async (companionId, bpm, beats, durationS) => {
      if (router === undefined) throw new Error("router is not ready");
      return await router.audioMetronome(companionId, bpm, beats, durationS);
    },
    tune: async (companionId, tempo, style, notes, title) => {
      if (router === undefined) throw new Error("router is not ready");
      return await router.audioTune(companionId, tempo, style, notes, title);
    },
    identify: async (companionId, seconds) => {
      if (router === undefined) throw new Error("router is not ready");
      return await router.audioIdentify(companionId, seconds);
    },
    stop: async (companionId) => {
      if (router === undefined) throw new Error("router is not ready");
      return await router.audioStop(companionId);
    },
  };
  const brain = await Brain.create({
    directory: config.directory,
    dataDir: config.opencodeDataDir,
    db,
    library,
    timers,
    audio,
    agentFile: config.agentFile,
    ...(provider === undefined ? {} : { provider }),
    ...(modelSpec === undefined
      ? {}
      : { models: { primary: modelSpec, fallback: modelSpec, deep: modelSpec } }),
    onLog: (line) => log(line),
    onEmit: (event) => {
      if (router === undefined) return;
      if (event.to !== undefined && event.to !== "" && event.say !== undefined) {
        void router.sayNow(event.to, event.say);
      } else if (event.say !== undefined) {
        const first = router.defaultCompanion();
        if (first !== undefined) void router.sayNow(first.id, event.say);
      }
    },
  });

  const edge = new EdgeClient({
    socketPath: config.edgeSock,
    log: (line) => log(line),
  });

  let routerRef: Router | undefined;
  const portraits = new PortraitManager({
    db,
    edge,
    dataDir: config.dataDir,
    log,
    onReady: async (companionId) => {
      await routerRef?.refreshCompanion(companionId);
    },
    announce: async (companionId, text) => {
      await routerRef?.sayNow(companionId, text);
    },
  });

  const remote = new RemoteManager({
    db,
    localPort: config.port,
    ...(config.tunnelBin === undefined ? {} : { bin: config.tunnelBin }),
    log,
  });

  const dc =
    config.disableDc || config.dcBin === undefined
      ? undefined
      : new DcService({
          db,
          edge,
          relay: config.relay,
          bin: config.dcBin,
          accountsDir: join(config.dataDir, "deltachat"),
          log,
          remote,
          onTurn: async (turn) => {
            if (routerRef === undefined) return undefined;
            return await routerRef.notifyDcTurn(turn);
          },
          onSay: async (companionId, text) => {
            await routerRef?.sayNow(companionId, text);
          },
          onStory: async (companionId, storyId) => {
            await routerRef?.openStory(companionId, storyId, false);
          },
        });

  router = new Router({
    db,
    edge,
    brain,
    dataDir: config.dataDir,
    portraits,
    identifySong: (wavPath) => identifySong(wavPath),
    ...(dc === undefined ? {} : { dc }),
    log,
  });
  routerRef = router;
  timers.start();

  const captures = new CaptureStore({
    dir: join(config.dataDir, "debug"),
    edge,
    terminalForCompanion: (companionId) => routerRef?.terminalForCompanion(companionId),
    companionForTerminal: (terminalId) => routerRef?.companionForTerminal(terminalId)?.id,
    log,
  });

  edge.start();
  try {
    await edge.waitReady(10_000);
  } catch (error) {
    log(`edge not ready: ${errMessage(error)}`);
  }

  if (dc !== undefined) {
    void dc.start().catch((error: unknown) => log(`Delta Chat: ${errMessage(error)}`));
  }

  let http: RunningHttp | undefined;
  try {
    http = await startHttp({
      host: config.host,
      port: config.port,
      adminToken: config.adminToken,
      dataDir: config.dataDir,
      db,
      edge,
      router,
      remote,
      captures,
      ...(dc === undefined ? {} : { dc }),
      portraits,
      libraryDir: config.libraryDir,
      opencode: true,
    });
  } catch (error) {
    await brain.close();
    void edge.close();
    db.close();
    library.close();
    throw error;
  }

  let closed = false;
  // Reverse startup order; a failing closer must not strand the rest.
  const closers: Array<() => Promise<void> | void> = [
    () => http?.close(),
    () => dc?.close(),
    () => router?.close(),
    () => edge.close(),
    () => brain.close(),
    () => remote.close(),
    () => library.close(),
    () => db.close(),
  ];
  return {
    config,
    db,
    edge,
    brain,
    router,
    dc,
    portraits,
    remote,
    captures,
    http,
    async close() {
      if (closed) return;
      closed = true;
      for (const close of closers) {
        try {
          await close();
        } catch (error) {
          log(`close: ${errMessage(error)}`);
        }
      }
    },
  };
}

export async function main(): Promise<void> {
  const runtime = await startRuntime({
    config: configFromEnv(),
    log: (line) => process.stderr.write(`lark-brain: ${line}\n`),
  });
  const shutdown = (signal: string): void => {
    process.stderr.write(`lark-brain: ${signal}, shutting down\n`);
    void runtime
      .close()
      .then(() => {
        process.exitCode = 0;
      })
      .catch((error: unknown) => {
        process.stderr.write(`lark-brain: shutdown failed: ${String(error)}\n`);
        process.exitCode = 1;
      });
  };
  process.once("SIGINT", () => shutdown("SIGINT"));
  process.once("SIGTERM", () => shutdown("SIGTERM"));
  process.stderr.write(
    `lark-brain: listening on ${runtime.config.host}:${runtime.http.port}, edge ${runtime.config.edgeSock}\n`,
  );
}

const invoked = process.argv[1] === undefined ? "" : resolve(process.argv[1]);
if (invoked === fileURLToPath(import.meta.url)) {
  await main().catch((error: unknown) => {
    process.stderr.write(`lark-brain: ${error instanceof Error ? error.stack ?? error.message : String(error)}\n`);
    process.exitCode = 1;
  });
}
