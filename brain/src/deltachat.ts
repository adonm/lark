// Delta Chat supervision and commands for the Lark brain. One Chatmail
// account per companion is provisioned on the configured relay; off-device
// traffic is text-only in both directions (inbound voice notes are decoded
// with ffmpeg and transcribed, never re-encoded to Opus).
import { execFile, spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { promisify } from "node:util";
import { StdioDeltaChat } from "@deltachat/jsonrpc-client";
import type { Companion, LarkDb } from "./db.ts";
import { parseTwee, slugFromTitle } from "./stories.ts";
import type { RemoteManager } from "./remote.ts";
import {
  contactKey,
  contactName,
  normalizeAddress,
  parseSlashCommand,
  runDcCommand,
  welcomeText,
} from "./dc_commands.ts";
import { errMessage } from "./util.ts";

export { contactName, parseSlashCommand } from "./dc_commands.ts";

const execFileAsync = promisify(execFile);

export const DEFAULT_CHATMAIL_RELAY = "nine.testrun.org";
export const DC_WELCOME_VERSION = "3";
export const DC_REMOVE_PENDING = "dc_remove_pending:";
export const DC_WELCOME_PREFIX = "dc_welcome:";
export const DC_RESTART_DELAY_MS = 2000;

export interface DcContact {
  id: number;
  address: string;
  displayName: string;
  name: string;
  isVerified: boolean;
  isBot: boolean;
  isBlocked: boolean;
}

export interface DcBasicChat {
  id: number;
  name: string;
  isContactRequest: boolean;
}

export interface DcMessage {
  id: number;
  chatId: number;
  fromId: number;
  text: string;
  viewType: string;
  file: string | null;
  isInfo: boolean;
  isBot: boolean;
}

export interface DcMessageData {
  text?: string;
  viewtype?: string;
  file?: string;
}

export interface DcRpc {
  addAccount(): Promise<number>;
  removeAccount(accountId: number): Promise<void>;
  getAllAccountIds(): Promise<number[]>;
  startIoForAllAccounts(): Promise<void>;
  addTransportFromQr(accountId: number, qr: string): Promise<void>;
  batchSetConfig(accountId: number, config: Record<string, string>): Promise<void>;
  setConfig(accountId: number, key: string, value: string | null): Promise<void>;
  getConfig(accountId: number, key: string): Promise<string | null>;
  getChatSecurejoinQrCode(accountId: number, chatId: number | null): Promise<string>;
  secureJoin(accountId: number, qr: string): Promise<number>;
  getContacts(accountId: number, listFlags: number, query: string | null): Promise<DcContact[]>;
  getContact(accountId: number, contactId: number): Promise<DcContact>;
  deleteContact(accountId: number, contactId: number): Promise<void>;
  getChatIdByContactId(accountId: number, contactId: number): Promise<number | null>;
  createChatByContactId(accountId: number, contactId: number): Promise<number>;
  deleteChat(accountId: number, chatId: number): Promise<void>;
  getBasicChatInfo(accountId: number, chatId: number): Promise<DcBasicChat>;
  getMessage(accountId: number, messageId: number): Promise<DcMessage>;
  sendMsg(accountId: number, chatId: number, data: DcMessageData): Promise<number>;
  /** Wire incoming-message and secure-join notifications. */
  subscribe(handlers: DcEventHandlers): void;
  close?(): void;
}

export interface DcEventHandlers {
  onIncomingMessage: (accountId: number, messageId: number) => void;
  onSecureJoinVerified: (accountId: number, contactId: number) => void;
}

export interface DcPeerTurn {
  companionId: string;
  text: string;
  sender: string;
  /** Lowercased contact address, stored on message rows to scope history. */
  peer: string;
  private: boolean;
  accountId?: number;
  chatId?: number;
}

export type DcTurnHandler = (turn: DcPeerTurn) => Promise<string | undefined>;

export interface DcServiceOptions {
  db: LarkDb;
  edge: { transcribe(path: string): Promise<string> };
  onTurn: DcTurnHandler;
  onSay?: (companionId: string, text: string) => Promise<void>;
  onStory?: (companionId: string, storyId: string) => Promise<void>;
  remote?: RemoteManager;
  relay?: string;
  rpc?: DcRpc;
  bin?: string;
  accountsDir: string;
  now?: () => number;
  log?: (line: string) => void;
  /** Test hook: return a 16 kHz mono WAV path for an audio attachment. */
  decodeVoice?: (path: string) => Promise<string>;
}

export class DeltaChatError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DeltaChatError";
  }
}

export function shortenDeltaChatInvite(raw: string): string {
  const index = raw.indexOf("#");
  if (index < 0) return raw;
  const head = raw.slice(0, index + 1);
  const parts = raw.slice(index + 1).split("&");
  if (parts.length <= 1) return raw;
  const kept = [parts[0] ?? ""];
  for (const part of parts.slice(1)) {
    if (part.startsWith("n=") || part.startsWith("v=")) continue;
    kept.push(part);
  }
  return head + kept.join("&");
}

async function defaultDecodeVoice(path: string): Promise<string> {
  const dir = mkdtempSync(join(tmpdir(), "lark-dc-voice-"));
  const out = join(dir, "note.wav");
  try {
    await execFileAsync(process.env.LARK_FFMPEG ?? "ffmpeg", [
      "-nostdin",
      "-y",
      "-i",
      path,
      "-ac",
      "1",
      "-ar",
      "16000",
      "-f",
      "wav",
      out,
    ]);
    return out;
  } catch (error) {
    rmSync(dir, { recursive: true, force: true });
    throw new DeltaChatError(`cannot decode voice note: ${errMessage(error)}`);
  }
}

interface AccountEntry {
  companionId: string;
  address: string;
}

export class DcService {
  readonly #options: DcServiceOptions;
  readonly #db: LarkDb;
  readonly #relay: string;
  readonly #now: () => number;
  readonly #log: (line: string) => void;
  readonly #decodeVoice: (path: string) => Promise<string>;
  readonly #accounts = new Map<number, AccountEntry>(); // account -> companion/address
  readonly #addresses = new Map<string, string>(); // address -> companion
  readonly #pendingRemovals = new Set<number>();
  readonly #tmpDirs = new Set<string>();
  readonly #accountsDir: string;
  readonly #bin: string | undefined;
  readonly #edge: DcServiceOptions["edge"];

  #rpc: DcRpc | undefined;
  #child: ChildProcessWithoutNullStreams | undefined;
  #restartTimer: ReturnType<typeof setTimeout> | undefined;
  #closed = false;
  #started = false;
  #ready = false;

  constructor(options: DcServiceOptions) {
    this.#options = options;
    this.#db = options.db;
    this.#relay = (options.relay ?? DEFAULT_CHATMAIL_RELAY).trim() || DEFAULT_CHATMAIL_RELAY;
    this.#now = options.now ?? (() => Date.now());
    this.#log = options.log ?? (() => undefined);
    this.#decodeVoice = options.decodeVoice ?? defaultDecodeVoice;
    this.#accountsDir = options.accountsDir;
    this.#bin = options.bin;
    this.#edge = options.edge;
    this.#rpc = options.rpc;
  }

  get ready(): boolean {
    return this.#ready;
  }

  get relay(): string {
    return this.#relay;
  }

  rpc(): DcRpc | undefined {
    return this.#rpc;
  }

  /** Start a supervised deltachat-rpc-server (production) or use injected RPC. */
  async start(): Promise<void> {
    if (this.#started) return;
    this.#started = true;
    if (this.#rpc !== undefined) {
      await this.attach(this.#rpc);
      return;
    }
    if (this.#bin === undefined || this.#bin === "") {
      this.#log("Delta Chat disabled: deltachat-rpc-server not found");
      return;
    }
    this.#spawn();
  }

  /** Attach an RPC transport (supervisor or test stub) and synchronise accounts. */
  async attach(rpc: DcRpc): Promise<void> {
    this.#rpc = rpc;
    rpc.subscribe({
      onIncomingMessage: (accountId, messageId) => {
        void this.handleMessage(accountId, messageId).catch((error: unknown) => {
          this.#log(`Delta Chat message: ${errMessage(error)}`);
        });
      },
      onSecureJoinVerified: (accountId, contactId) => {
        void this.handleSecureJoinVerified(accountId, contactId).catch(() => undefined);
      },
    });
    try {
      await rpc.startIoForAllAccounts();
    } catch (error) {
      this.#log(`Delta Chat start IO: ${errMessage(error)}`);
    }
    this.#ready = true;
    await this.syncAccounts();
  }

  #spawn(): void {
    if (this.#closed) return;
    const bin = this.#bin;
    if (bin === undefined) return;
    let child: ChildProcessWithoutNullStreams;
    try {
      child = spawn(bin, [], {
        env: { ...process.env, DC_ACCOUNTS_PATH: this.#accountsDir },
        stdio: ["pipe", "pipe", "pipe"],
      });
    } catch (error) {
      this.#log(`Delta Chat spawn: ${errMessage(error)}`);
      this.#scheduleRestart();
      return;
    }
    this.#child = child;
    child.stderr.on("data", (chunk: Buffer) => this.#log(`deltachat-rpc-server: ${chunk.toString().trim()}`));
    const dc = new StdioDeltaChat(child.stdin, child.stdout, true);
    const adapter = stdioDcAdapter(dc as unknown as DcClient);
    void this.attach(adapter).catch((error: unknown) => {
      this.#log(`Delta Chat attach: ${errMessage(error)}`);
    });
    child.on("exit", (code, signal) => {
      this.#ready = false;
      this.#rpc = undefined;
      dc.close();
      this.#log(`Delta Chat rpc-server exited (${String(code ?? signal)}); restarting`);
      this.#scheduleRestart();
    });
  }

  #scheduleRestart(): void {
    if (this.#closed || this.#restartTimer !== undefined) return;
    this.#restartTimer = setTimeout(() => {
      this.#restartTimer = undefined;
      this.#spawn();
    }, DC_RESTART_DELAY_MS);
    this.#restartTimer.unref();
  }

  async close(): Promise<void> {
    this.#closed = true;
    this.#ready = false;
    if (this.#restartTimer !== undefined) clearTimeout(this.#restartTimer);
    this.#restartTimer = undefined;
    this.#rpc?.close?.();
    this.#rpc = undefined;
    const child = this.#child;
    this.#child = undefined;
    if (child !== undefined) {
      child.kill("SIGTERM");
    }
    for (const dir of this.#tmpDirs) rmSync(dir, { recursive: true, force: true });
    this.#tmpDirs.clear();
  }

  #requireRpc(): DcRpc {
    if (this.#rpc === undefined) throw new DeltaChatError("Delta Chat unavailable");
    return this.#rpc;
  }

  // ---- provisioning -------------------------------------------------------

  async syncAccounts(): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    await this.retryPendingRemovals(rpc);
    for (const companion of this.#db.listCompanions()) {
      try {
        await this.ensureCompanion(companion);
      } catch (error) {
        this.#log(`Delta Chat account for ${companion.id}: ${errMessage(error)}`);
      }
    }
    await this.autoPairAll();
    await this.syncWelcomes();
  }

  async ensureCompanion(companion: Companion): Promise<number> {
    const rpc = this.#requireRpc();
    let accountId = companion.dcAccountId;
    if (accountId === 0) {
      accountId = await rpc.addAccount();
      try {
        await this.provisionAccount(rpc, companion, accountId);
      } catch (error) {
        await this.dropAccount(accountId);
        throw error;
      }
      this.#db.patchCompanion(companion.id, { dcAccountId: accountId });
    }
    const address = normalizeAddress((await rpc.getConfig(accountId, "addr")) ?? "");
    this.#accounts.set(accountId, { companionId: companion.id, address });
    if (address !== "") this.#addresses.set(address, companion.id);
    try {
      await rpc.setConfig(accountId, "displayname", companion.name);
    } catch (error) {
      this.#log(`Delta Chat displayname for ${companion.id}: ${errMessage(error)}`);
    }
    return accountId;
  }

  async provisionAccount(rpc: DcRpc, companion: Companion, accountId: number): Promise<void> {
    await rpc.batchSetConfig(accountId, { bot: "1", displayname: companion.name });
    await rpc.setConfig(accountId, "who_can_call_me", "nobody");
    await rpc.addTransportFromQr(accountId, `dcaccount:${this.#relay}`);
    try {
      await rpc.startIoForAllAccounts();
    } catch (error) {
      this.#log(`Delta Chat start IO after provisioning ${companion.id}: ${String(error)}`);
    }
  }

  async dropAccount(accountId: number): Promise<void> {
    if (accountId === 0) return;
    const rpc = this.#rpc;
    if (rpc !== undefined) {
      try {
        await rpc.removeAccount(accountId);
        this.clearPendingRemoval(accountId);
        return;
      } catch (error) {
        this.#log(`Delta Chat could not remove account ${accountId}: ${String(error)}`);
      }
    }
    this.#db.setMeta(`${DC_REMOVE_PENDING}${accountId}`, new Date(this.#now()).toISOString());
    this.#pendingRemovals.add(accountId);
  }

  clearPendingRemoval(accountId: number): void {
    this.#db.deleteMeta(`${DC_REMOVE_PENDING}${accountId}`);
    this.#pendingRemovals.delete(accountId);
  }

  async retryPendingRemovals(rpc: DcRpc = this.#requireRpc()): Promise<void> {
    for (const key of this.#db.listMetaKeys(`${DC_REMOVE_PENDING}%`)) {
      const raw = key.slice(DC_REMOVE_PENDING.length);
      const accountId = Number.parseInt(raw, 10);
      if (!Number.isInteger(accountId) || accountId <= 0) continue;
      try {
        await rpc.removeAccount(accountId);
        this.clearPendingRemoval(accountId);
      } catch (error) {
        this.#log(`Delta Chat could not remove account ${accountId} yet: ${String(error)}`);
      }
    }
  }

  async autoPairAll(): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    const peers: Array<{ companionId: string; accountId: number; address: string }> = [];
    for (const companion of this.#db.listCompanions()) {
      if (companion.dcAccountId === 0) continue;
      let address = "";
      try {
        address = normalizeAddress((await rpc.getConfig(companion.dcAccountId, "addr")) ?? "");
      } catch {
        continue;
      }
      if (address === "") continue;
      peers.push({ companionId: companion.id, accountId: companion.dcAccountId, address });
    }
    for (let i = 0; i < peers.length; i++) {
      const a = peers[i];
      if (a === undefined) continue;
      let contacts: DcContact[] = [];
      try {
        contacts = await rpc.getContacts(a.accountId, 0, null);
      } catch {
        continue;
      }
      const verified = new Set(contacts.filter((c) => c.isVerified).map((c) => normalizeAddress(c.address)));
      for (let j = i + 1; j < peers.length; j++) {
        const b = peers[j];
        if (b === undefined || verified.has(b.address)) continue;
        try {
          const qr = await rpc.getChatSecurejoinQrCode(a.accountId, null);
          await rpc.secureJoin(b.accountId, qr);
        } catch (error) {
          this.#log(`Delta Chat SecureJoin ${b.companionId} -> ${a.companionId}: ${String(error)}`);
        }
      }
    }
  }

  async syncWelcomes(): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    for (const companion of this.#db.listCompanions()) {
      if (companion.dcAccountId === 0) continue;
      let contacts: DcContact[] = [];
      try {
        contacts = await rpc.getContacts(companion.dcAccountId, 0, null);
      } catch {
        continue;
      }
      for (const contact of contacts) {
        await this.sendWelcomeIfNeeded(companion, companion.dcAccountId, contact);
      }
    }
  }

  async sendWelcomeIfNeeded(companion: Companion, accountId: number, contact: DcContact): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    if (contact.id === 1 || !contact.isVerified || contact.isBlocked || contact.isBot) return;
    if (this.#addresses.has(normalizeAddress(contact.address))) return;
    const key = `${DC_WELCOME_PREFIX}${companion.id}:${contactKey(contact)}`;
    if (this.#db.getMeta(key) === DC_WELCOME_VERSION) return;
    const chatId = await rpc.createChatByContactId(accountId, contact.id);
    await rpc.sendMsg(accountId, chatId, { text: welcomeText(companion) });
    this.#db.setMeta(key, DC_WELCOME_VERSION);
  }

  async handleSecureJoinVerified(accountId: number, contactId: number): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    const entry = this.#accounts.get(accountId) ?? (await this.lookupAccount(accountId));
    if (entry === undefined) return;
    const companion = this.#db.getCompanion(entry.companionId);
    if (companion === undefined) return;
    const contact = await rpc.getContact(accountId, contactId);
    await this.sendWelcomeIfNeeded(companion, accountId, contact);
  }

  async lookupAccount(accountId: number): Promise<AccountEntry | undefined> {
    const cached = this.#accounts.get(accountId);
    if (cached !== undefined) return cached;
    for (const companion of this.#db.listCompanions()) {
      if (companion.dcAccountId === accountId) {
        const address = normalizeAddress((await this.#requireRpc().getConfig(accountId, "addr")) ?? "");
        const entry = { companionId: companion.id, address };
        this.#accounts.set(accountId, entry);
        if (address !== "") this.#addresses.set(address, companion.id);
        return entry;
      }
    }
    return undefined;
  }

  companionByAddress(address: string): string | undefined {
    return this.#addresses.get(normalizeAddress(address));
  }

  accountForCompanion(companionId: string): number {
    const companion = this.#db.getCompanion(companionId);
    if (companion !== undefined && companion.dcAccountId !== 0) return companion.dcAccountId;
    for (const [accountId, entry] of this.#accounts) {
      if (entry.companionId === companionId) return accountId;
    }
    return 0;
  }

  forget(companionId: string): void {
    for (const [accountId, entry] of this.#accounts) {
      if (entry.companionId !== companionId) continue;
      this.#accounts.delete(accountId);
      if (entry.address !== "") this.#addresses.delete(entry.address);
    }
  }

  async removeCompanion(companionId: string): Promise<void> {
    const accountId = this.accountForCompanion(companionId);
    this.forget(companionId);
    if (accountId !== 0) await this.dropAccount(accountId);
  }

  // ---- inbound ------------------------------------------------------------

  async handleMessage(accountId: number, messageId: number): Promise<void> {
    const rpc = this.#rpc;
    if (rpc === undefined) return;
    const entry = this.#accounts.get(accountId) ?? (await this.lookupAccount(accountId));
    if (entry === undefined) return;
    const companion = this.#db.getCompanion(entry.companionId);
    if (companion === undefined) return;

    const message = await rpc.getMessage(accountId, messageId);
    if (message.isInfo || message.fromId === 0 || message.fromId === 1) return;
    const contact = await rpc.getContact(accountId, message.fromId);
    if (!contact.isVerified || contact.isBlocked) return;
    const chat = await rpc.getBasicChatInfo(accountId, message.chatId);
    if (chat.isContactRequest) return;

    const peerId = this.companionByAddress(contact.address);
    if (peerId !== undefined) {
      // Companion-to-companion traffic is mesh bookkeeping, not a user turn.
      return;
    }

    const voice = message.viewType === "Voice" || message.viewType === "Audio";
    let text = message.text.trim();
    if (voice) {
      text = await this.transcribeVoice(message);
      if (text === "") return;
    }
    const sender = contactName(contact);
    const peer = normalizeAddress(contact.address);
    const file = message.file ?? "";
    if (!voice && file.toLowerCase().endsWith(".twee")) {
      await this.importTweeAttachment(rpc, accountId, message.chatId, sender, file);
      return;
    }
    if (text === "") return;

    let privateTurn = false;
    if (!voice && text.startsWith("/")) {
      const outcome = await this.runCommand(rpc, accountId, message.chatId, companion, contact, parseSlashCommand(text));
      if (outcome.handled) return;
      privateTurn = outcome.private;
      text = outcome.text;
    }

    const userTs = this.#now();
    this.#db.appendMessage({
      id: `dc:${accountId}:${message.id}:user`,
      companionId: companion.id,
      role: "user",
      sender,
      text,
      source: "deltachat",
      channel: "deltachat",
      cardTitle: "",
      tsMs: userTs,
      peer,
    });
    try {
      const reply = await this.#options.onTurn({
        companionId: companion.id,
        text,
        sender,
        peer,
        private: privateTurn,
        accountId,
        chatId: message.chatId,
      });
      if (reply === undefined || reply.trim() === "") return;
      this.#db.appendMessage({
        id: `dc:${accountId}:${message.id}:reply`,
        companionId: companion.id,
        role: "assistant",
        sender: companion.name,
        text: reply,
        source: "deltachat",
        channel: "deltachat",
        cardTitle: "",
        tsMs: Math.max(this.#now(), userTs + 1),
        peer,
      });
      await this.sendText(accountId, message.chatId, reply);
    } catch (error) {
      this.#log(`Delta Chat turn: ${errMessage(error)}`);
    }
  }

  async transcribeVoice(message: DcMessage): Promise<string> {
    const file = message.file;
    if (file === null || file === "") return "";
    const wav = await this.#decodeVoice(file);
    const dir = dirname(wav);
    if (wav.startsWith(tmpdir())) this.#tmpDirs.add(dir);
    try {
      return (await this.#edge.transcribe(wav)).trim();
    } finally {
      rmSync(wav, { force: true });
      // Only remove the decoder's own temporary directory; an injected test
      // decoder may return a path inside the caller's workspace.
      if (dir.startsWith(join(tmpdir(), "lark-dc-voice-"))) rmSync(dir, { recursive: true, force: true });
    }
  }

  async importTweeAttachment(rpc: DcRpc, accountId: number, chatId: number, sender: string, file: string): Promise<void> {
    if (readFileSync(file).byteLength > 1 << 20) {
      await rpc.sendMsg(accountId, chatId, { text: "That story file is too large." });
      return;
    }
    const raw = readFileSync(file, "utf8");
    const parsed = parseTwee(raw);
    const id = slugFromTitle(parsed.title);
    this.#db.saveStory({
      id,
      title: parsed.title,
      twee: raw,
      author: sender,
      updatedAt: this.#now(),
      passages: parsed.passages.length,
    });
    await rpc.sendMsg(accountId, chatId, {
      text: `Imported “${parsed.title}” (${parsed.passages.length} passages).`,
    });
  }

  async runCommand(
    rpc: DcRpc,
    accountId: number,
    chatId: number,
    companion: Companion,
    contact: DcContact,
    command: { name: string; arg: string },
  ): Promise<{ handled: boolean; private: boolean; text: string }> {
    return await runDcCommand(
      {
        db: this.#db,
        ...(this.#options.remote === undefined ? {} : { remote: this.#options.remote }),
        ...(this.#options.onSay === undefined ? {} : { onSay: this.#options.onSay }),
        ...(this.#options.onStory === undefined ? {} : { onStory: this.#options.onStory }),
      },
      rpc,
      accountId,
      chatId,
      companion,
      contact,
      command,
    );
  }

  async sendText(accountId: number, chatId: number, text: string): Promise<void> {
    await this.#requireRpc().sendMsg(accountId, chatId, { text });
  }

  // ---- pairing ------------------------------------------------------------

  async pairInvite(companionId: string): Promise<string> {
    const accountId = this.accountForCompanion(companionId);
    if (accountId === 0) throw new DeltaChatError("Companion has no Delta Chat account");
    const qr = await this.#requireRpc().getChatSecurejoinQrCode(accountId, null);
    return shortenDeltaChatInvite(qr);
  }

  async listPairedContacts(companionId: string): Promise<DcContact[]> {
    const accountId = this.accountForCompanion(companionId);
    if (accountId === 0) throw new DeltaChatError("Companion has no Delta Chat account");
    const contacts = await this.#requireRpc().getContacts(accountId, 0, null);
    return contacts
      .filter((c) => c.id !== 1 && c.isVerified && !c.isBlocked && this.companionByAddress(c.address) === undefined)
      .sort((a, b) => contactName(a).localeCompare(contactName(b), undefined, { sensitivity: "base" }));
  }


  async unpairContact(companionId: string, contactId: number): Promise<void> {
    const accountId = this.accountForCompanion(companionId);
    if (accountId === 0) throw new DeltaChatError("Companion has no Delta Chat account");
    const rpc = this.#requireRpc();
    const chatId = await rpc.getChatIdByContactId(accountId, contactId);
    if (chatId !== null) {
      try {
        await rpc.deleteChat(accountId, chatId);
      } catch (error) {
        this.#log(`Delta Chat delete chat ${chatId}: ${String(error)}`);
      }
    }
    await rpc.deleteContact(accountId, contactId);
  }

  // ---- diagnostics --------------------------------------------------------

  async reconcile(): Promise<{ onDisk: number; linked: number; orphans: number[]; pendingRemoval: number }> {
    const linked = new Set(
      this.#db
        .listCompanions()
        .map((c) => c.dcAccountId)
        .filter((id) => id !== 0),
    );
    const rpc = this.#rpc;
    const ids = rpc === undefined ? [] : await rpc.getAllAccountIds();
    const orphans = ids.filter((id) => !linked.has(id)).sort((a, b) => a - b);
    return {
      onDisk: ids.length,
      linked: linked.size,
      orphans,
      pendingRemoval: this.#db.listMetaKeys(`${DC_REMOVE_PENDING}%`).length,
    };
  }
}

// ---- @deltachat/jsonrpc-client adapter ------------------------------------

interface IncomingEvent {
  kind: string;
  msgId?: number;
  contactId?: number;
  progress?: number;
}

interface DcClient {
  rpc: {
    addAccount(): Promise<number>;
    removeAccount(id: number): Promise<null>;
    getAllAccountIds(): Promise<number[]>;
    startIoForAllAccounts(): Promise<null>;
    addTransportFromQr(id: number, qr: string): Promise<null>;
    batchSetConfig(id: number, config: Record<string, string | null>): Promise<null>;
    setConfig(id: number, key: string, value: string | null): Promise<null>;
    getConfig(id: number, key: string): Promise<string | null>;
    getChatSecurejoinQrCode(id: number, chat: number | null): Promise<string>;
    secureJoin(id: number, qr: string): Promise<number>;
    getContacts(id: number, flags: number, query: string | null): Promise<DcContact[]>;
    getContact(id: number, contact: number): Promise<DcContact>;
    deleteContact(id: number, contact: number): Promise<null>;
    getChatIdByContactId(id: number, contact: number): Promise<number | null>;
    createChatByContactId(id: number, contact: number): Promise<number>;
    deleteChat(id: number, chat: number): Promise<null>;
    getBasicChatInfo(id: number, chat: number): Promise<DcBasicChat>;
    getMessage(id: number, message: number): Promise<DcMessage>;
    sendMsg(id: number, chat: number, data: { text?: string | null; viewtype?: string; file?: string }): Promise<number>;
  };
  on(event: string, listener: (accountId: number, event: IncomingEvent) => void): void;
  close(): void;
}

/**
 * Bridge the stdio JSON-RPC client to DcRpc. Pure RPCs fall through the proxy;
 * only subscription, close, and the message/chat shapes the wire client models
 * differently are overridden.
 */
export function stdioDcAdapter(dc: DcClient): DcRpc {
  const overrides: Partial<DcRpc> = {
    subscribe(handlers): void {
      dc.on("IncomingMsg", (accountId, event) => {
        if (typeof event.msgId === "number") handlers.onIncomingMessage(accountId, event.msgId);
      });
      dc.on("SecurejoinInviterProgress", (accountId, event) => {
        if (typeof event.contactId !== "number") return;
        if (typeof event.progress === "number" && event.progress < 1000) return;
        handlers.onSecureJoinVerified(accountId, event.contactId);
      });
    },
    close(): void {
      dc.close();
    },
    async getBasicChatInfo(accountId, chatId): Promise<DcBasicChat> {
      const chat = await dc.rpc.getBasicChatInfo(accountId, chatId);
      return { id: chat.id, name: chat.name, isContactRequest: chat.isContactRequest };
    },
    async getMessage(accountId, messageId): Promise<DcMessage> {
      const message = await dc.rpc.getMessage(accountId, messageId);
      return {
        id: message.id,
        chatId: message.chatId,
        fromId: message.fromId,
        text: message.text,
        viewType: message.viewType,
        file: message.file,
        isInfo: message.isInfo,
        isBot: message.isBot,
      };
    },
    async sendMsg(accountId, chatId, data): Promise<number> {
      return await dc.rpc.sendMsg(accountId, chatId, {
        text: data.text ?? null,
        ...(data.viewtype === undefined ? {} : { viewtype: data.viewtype }),
        ...(data.file === undefined ? {} : { file: data.file }),
      });
    },
  };
  return new Proxy(overrides as DcRpc, {
    get(target, property, receiver) {
      if (Reflect.has(target, property)) return Reflect.get(target, property, receiver);
      const value = Reflect.get(dc.rpc, property);
      return typeof value === "function" ? value.bind(dc.rpc) : value;
    },
  });
}
