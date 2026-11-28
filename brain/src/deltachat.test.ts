import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { LarkDb } from "./db.ts";
import {
  DC_WELCOME_VERSION,
  DcService,
  contactName,
  parseSlashCommand,
  shortenDeltaChatInvite,
  stdioDcAdapter,
  type DcBasicChat,
  type DcContact,
  type DcEventHandlers,
  type DcMessage,
  type DcMessageData,
  type DcRpc,
  type StdioDcSource,
} from "./deltachat.ts";
import { RemoteManager } from "./remote.ts";

interface Account {
  id: number;
  address: string;
  config: Map<string, string>;
  contacts: Map<number, DcContact>;
  messages: Map<number, DcMessage>;
  chats: Map<number, DcBasicChat>;
}

class StubRpc implements DcRpc {
  readonly accounts = new Map<number, Account>();
  readonly sent: Array<{ accountId: number; chatId: number; data: DcMessageData }> = [];
  readonly events = new Map<number, DcContact[]>();
  readonly secureJoins: Array<{ accountId: number; qr: string }> = [];
  handlers: DcEventHandlers | undefined;
  nextAccount = 0;
  nextMessage = 0;
  nextContact = 10;
  failRemovals = 0;

  account(id: number): Account {
    const account = this.accounts.get(id);
    if (account === undefined) throw new Error(`no account ${id}`);
    return account;
  }

  addContact(accountId: number, input: Partial<DcContact> & { address: string }): DcContact {
    const contact: DcContact = {
      id: input.id ?? this.nextContact++,
      address: input.address,
      displayName: input.displayName ?? input.name ?? input.address,
      name: input.name ?? input.displayName ?? input.address,
      isVerified: input.isVerified ?? true,
      isBot: input.isBot ?? false,
      isBlocked: input.isBlocked ?? false,
    };
    this.account(accountId).contacts.set(contact.id, contact);
    return contact;
  }

  addIncoming(accountId: number, input: { from: DcContact; chatId?: number; text?: string; viewType?: string; file?: string | null }): DcMessage {
    const account = this.account(accountId);
    const id = ++this.nextMessage;
    const chatId = input.chatId ?? 100 + input.from.id;
    const message: DcMessage = {
      id,
      chatId,
      fromId: input.from.id,
      text: input.text ?? "",
      viewType: input.viewType ?? "Text",
      file: input.file ?? null,
      isInfo: false,
      isBot: false,
    };
    account.messages.set(id, message);
    account.chats.set(chatId, { id: chatId, name: contactName(input.from), isContactRequest: false });
    return message;
  }

  emitIncoming(accountId: number, message: DcMessage): void {
    this.handlers?.onIncomingMessage(accountId, message.id);
  }

  async addAccount(): Promise<number> {
    const id = ++this.nextAccount;
    this.accounts.set(id, {
      id,
      address: `companion${id}@nine.testrun.org`,
      config: new Map(),
      contacts: new Map(),
      messages: new Map(),
      chats: new Map(),
    });
    return id;
  }

  async removeAccount(accountId: number): Promise<void> {
    if (this.failRemovals > 0) {
      this.failRemovals--;
      throw new Error("rpc busy");
    }
    this.accounts.delete(accountId);
  }

  async getAllAccountIds(): Promise<number[]> {
    return [...this.accounts.keys()].sort((a, b) => a - b);
  }

  async startIoForAllAccounts(): Promise<void> {}

  async addTransportFromQr(accountId: number, qr: string): Promise<void> {
    this.account(accountId).config.set("transport", qr);
  }

  async batchSetConfig(accountId: number, config: Record<string, string>): Promise<void> {
    for (const [key, value] of Object.entries(config)) this.account(accountId).config.set(key, value);
  }

  async setConfig(accountId: number, key: string, value: string | null): Promise<void> {
    if (value === null) this.account(accountId).config.delete(key);
    else this.account(accountId).config.set(key, value);
  }

  async getConfig(accountId: number, key: string): Promise<string | null> {
    if (key === "addr") return this.account(accountId).address;
    return this.account(accountId).config.get(key) ?? null;
  }

  async createQrSvg(text: string): Promise<string> {
    return `<svg>${text}</svg>`;
  }

  async getChatSecurejoinQrCode(accountId: number, _chatId: number | null): Promise<string> {
    return `https://i.delta.chat/#invite&a=${encodeURIComponent(this.account(accountId).address)}&n=Name&v=1`;
  }

  async secureJoin(accountId: number, qr: string): Promise<number> {
    this.secureJoins.push({ accountId, qr });
    const match = /a=([^&]+)/.exec(qr);
    const address = match?.[1] ?? "peer@example.org";
    const contact = this.addContact(accountId, { address: decodeURIComponent(address) });
    return 100 + contact.id;
  }

  async getContacts(accountId: number, _flags: number, _query: string | null): Promise<DcContact[]> {
    return [...this.account(accountId).contacts.values()];
  }

  async getContact(accountId: number, contactId: number): Promise<DcContact> {
    const contact = this.account(accountId).contacts.get(contactId);
    if (contact === undefined) throw new Error("contact not found");
    return contact;
  }

  async deleteContact(accountId: number, contactId: number): Promise<void> {
    this.account(accountId).contacts.delete(contactId);
  }

  async getChatIdByContactId(accountId: number, contactId: number): Promise<number | null> {
    for (const chat of this.account(accountId).chats.values()) {
      if (chat.name === contactName(this.account(accountId).contacts.get(contactId) ?? {
        id: contactId, address: "", displayName: "", name: "", isVerified: true, isBot: false, isBlocked: false,
      })) return chat.id;
    }
    return null;
  }

  async createChatByContactId(accountId: number, contactId: number): Promise<number> {
    const chatId = 1000 + contactId;
    const contact = await this.getContact(accountId, contactId);
    this.account(accountId).chats.set(chatId, { id: chatId, name: contactName(contact), isContactRequest: false });
    return chatId;
  }

  async deleteChat(accountId: number, chatId: number): Promise<void> {
    this.account(accountId).chats.delete(chatId);
  }

  async getBasicChatInfo(accountId: number, chatId: number): Promise<DcBasicChat> {
    return this.account(accountId).chats.get(chatId) ?? { id: chatId, name: "Chat", isContactRequest: false };
  }

  async getMessage(accountId: number, messageId: number): Promise<DcMessage> {
    const message = this.account(accountId).messages.get(messageId);
    if (message === undefined) throw new Error("message not found");
    return message;
  }

  async sendMsg(accountId: number, chatId: number, data: DcMessageData): Promise<number> {
    this.sent.push({ accountId, chatId, data });
    return ++this.nextMessage;
  }

  subscribe(handlers: DcEventHandlers): void {
    this.handlers = handlers;
  }
}

interface Harness {
  dir: string;
  db: LarkDb;
  rpc: StubRpc;
  service: DcService;
  turns: Array<{ companionId: string; text: string; sender: string; private: boolean }>;
  said: Array<string>;
  opened: Array<string>;
}

function harness(): Harness {
  const dir = mkdtempSync(join(tmpdir(), "lark-dc-test-"));
  const db = new LarkDb(join(dir, "lark.db"));
  const rpc = new StubRpc();
  const turns: Harness["turns"] = [];
  const said: string[] = [];
  const opened: string[] = [];
  const remote = new RemoteManager({
    db,
    localPort: 8765,
    runner: async () => ({ stdout: "https://greg.opentunnel.test\n", stderr: "", code: 0 }),
  });
  const service = new DcService({
    db,
    edge: { transcribe: async () => "voice transcription" },
    accountsDir: join(dir, "deltachat"),
    relay: "nine.testrun.org",
    rpc,
    remote,
    log: () => undefined,
    decodeVoice: async () => join(dir, "note.wav"),
    onTurn: async (turn) => {
      turns.push(turn);
      return `reply to ${turn.text}`;
    },
    onSay: async (_companionId, text) => {
      said.push(text);
    },
    onStory: async (_companionId, storyId) => {
      opened.push(storyId);
    },
  });
  return { dir, db, rpc, service, turns, said, opened };
}

async function ready(h: Harness): Promise<void> {
  await h.service.attach(h.rpc);
}

test("parseSlashCommand splits name and argument", () => {
  assert.deepEqual(parseSlashCommand("/say hello there"), { name: "/say", arg: "hello there" });
  assert.deepEqual(parseSlashCommand("  /REMOTE off "), { name: "/remote", arg: "off" });
  assert.deepEqual(parseSlashCommand("hello"), { name: "", arg: "hello" });
});

test("shortenDeltaChatInvite drops display name and version", () => {
  assert.equal(
    shortenDeltaChatInvite("https://i.delta.chat/#abc&a=x&n=Greg&v=3&b=y"),
    "https://i.delta.chat/#abc&a=x&b=y",
  );
  assert.equal(shortenDeltaChatInvite("https://i.delta.chat/#abc"), "https://i.delta.chat/#abc");
});

test("provisioning creates one account, relay transport, and one versioned welcome", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", configured: true });
  await h.service.attach(h.rpc);
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  await h.service.syncAccounts();
  const greg = h.db.getCompanion("greg");
  assert.equal(greg?.dcAccountId, 1);
  assert.equal(h.rpc.account(1).config.get("bot"), "1");
  assert.equal(h.rpc.account(1).config.get("transport"), "dcaccount:nine.testrun.org");
  assert.equal(h.rpc.account(1).config.get("who_can_call_me"), "nobody");
  assert.equal(h.db.getMeta("dc_welcome:greg:sam@example.org"), DC_WELCOME_VERSION);
  const welcome = h.rpc.sent.find((entry) => entry.data.text?.includes("Hi! I'm Greg, an old chameleon."));
  assert.ok(welcome !== undefined);
  assert.ok(welcome.data.text?.includes("/remote off"));
  assert.equal(h.rpc.sent.filter((entry) => entry.data.text?.includes("Text me anytime")).length, 1);

  // A repeated sync must not send the welcome twice.
  await h.service.syncAccounts();
  assert.equal(h.rpc.sent.filter((entry) => entry.data.text?.includes("Text me anytime")).length, 1);
  assert.equal(sam.id, 10);
});

test("verified contact text round trip is text-only and recorded", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  await ready(h);
  const message = h.rpc.addIncoming(1, { from: sam, text: "hello Greg" });
  await h.service.handleMessage(1, message.id);
  assert.deepEqual(h.turns.map((turn) => turn.text), ["hello Greg"]);
  const reply = h.rpc.sent.find((entry) => entry.data.text === "reply to hello Greg");
  assert.ok(reply !== undefined);
  assert.equal(reply.data.viewtype, undefined);
  assert.equal(reply.data.file, undefined);
  const rows = h.db.listMessages({ companionId: "greg", channel: "deltachat" });
  assert.equal(rows.messages.length, 2);
  assert.equal(rows.messages[0]?.role, "user");
  assert.equal(rows.messages[0]?.sender, "Sam");
  assert.equal(rows.messages[0]?.peer, "sam@example.org");
  assert.equal(rows.messages[1]?.role, "assistant");
  assert.equal(rows.messages[1]?.peer, "sam@example.org");
  assert.ok((rows.messages[1]?.tsMs ?? 0) >= (rows.messages[0]?.tsMs ?? 0));
});

test("unverified, blocked, info, and peer messages are ignored", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const unverified = h.rpc.addContact(1, { address: "spam@example.org", isVerified: false });
  const blocked = h.rpc.addContact(1, { address: "blocked@example.org", isBlocked: true });
  await h.rpc.addAccount();
  h.db.upsertCompanion({ id: "cake", name: "Cake", dcAccountId: 2 });
  const peer = h.rpc.addContact(1, { address: "companion2@nine.testrun.org", displayName: "Cake" });
  await ready(h);
  const a = h.rpc.addIncoming(1, { from: unverified, text: "spam" });
  const b = h.rpc.addIncoming(1, { from: blocked, text: "blocked" });
  const c = h.rpc.addIncoming(1, { from: peer, text: "peer" });
  await h.service.handleMessage(1, a.id);
  await h.service.handleMessage(1, b.id);
  await h.service.handleMessage(1, c.id);
  assert.equal(h.turns.length, 0);
});

test("voice notes decode and transcribe instead of sending Opus", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  await ready(h);
  const message = h.rpc.addIncoming(1, { from: sam, viewType: "Voice", file: "/tmp/note.ogg" });
  await h.service.handleMessage(1, message.id);
  assert.deepEqual(h.turns.map((turn) => turn.text), ["voice transcription"]);
  assert.ok(h.rpc.sent.every((entry) => entry.data.file === undefined));
});

test("slash commands route /say, /private, /help, /stories, /story, /remote and /remote off", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", form: "an old chameleon", configured: true });
  h.db.saveStory({ id: "lantern", title: "Lantern", twee: ":: StoryTitle\nLantern\n\n:: Start {\"end\": true}\nA light.\n", author: "", updatedAt: 1, passages: 1 });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  await ready(h);

  const say = h.rpc.addIncoming(1, { from: sam, text: "/say hello room" });
  await h.service.handleMessage(1, say.id);
  assert.deepEqual(h.said, ["hello room"]);
  assert.equal(h.rpc.sent.at(-1)?.data.text, "Said it.");

  const privateMessage = h.rpc.addIncoming(1, { from: sam, text: "/private just us" });
  await h.service.handleMessage(1, privateMessage.id);
  assert.equal(h.turns.at(-1)?.private, true);
  assert.equal(h.turns.at(-1)?.text, "just us");

  const help = h.rpc.addIncoming(1, { from: sam, text: "/help" });
  await h.service.handleMessage(1, help.id);
  assert.ok(h.rpc.sent.at(-1)?.data.text?.includes("/say <words>"));

  const stories = h.rpc.addIncoming(1, { from: sam, text: "/stories" });
  await h.service.handleMessage(1, stories.id);
  assert.ok(h.rpc.sent.at(-1)?.data.text?.includes("Lantern (lantern, 1 passages)"));

  const story = h.rpc.addIncoming(1, { from: sam, text: "/story lantern" });
  await h.service.handleMessage(1, story.id);
  assert.deepEqual(h.opened, ["lantern"]);
  assert.equal(h.rpc.sent.at(-1)?.data.text, "Opening “Lantern”.");

  const remote = h.rpc.addIncoming(1, { from: sam, text: "/remote" });
  await h.service.handleMessage(1, remote.id);
  const link = h.rpc.sent.at(-1)?.data.text ?? "";
  assert.match(link, /^https:\/\/greg\.opentunnel\.test\/remote\?token=/);

  const off = h.rpc.addIncoming(1, { from: sam, text: "/remote off" });
  await h.service.handleMessage(1, off.id);
  assert.equal(h.rpc.sent.at(-1)?.data.text, "Remote controls off.");
});

test(".twee attachments import a validated story", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  await ready(h);
  const path = join(h.dir, "moon.twee");
  writeFileSync(path, ":: StoryTitle\nMoon\n\n:: Start {\"end\": true}\nA quiet moon.\n");
  const message = h.rpc.addIncoming(1, { from: sam, file: path });
  await h.service.handleMessage(1, message.id);
  const story = h.db.getStory("moon");
  assert.equal(story?.title, "Moon");
  assert.equal(h.rpc.sent.at(-1)?.data.text, "Imported “Moon” (1 passages).");
});

test("failed account removal is durable and retried on the next sync", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  h.rpc.failRemovals = 1;
  await h.service.dropAccount(1);
  assert.ok(h.db.getMeta("dc_remove_pending:1") !== undefined);
  await ready(h);
  assert.equal(h.db.getMeta("dc_remove_pending:1"), undefined);
  assert.equal(h.rpc.accounts.has(1), false);
});

test("auto-pair joins the mesh with secure-join QR codes", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  h.db.upsertCompanion({ id: "cake", name: "Cake", configured: true });
  await h.rpc.addAccount();
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  h.db.patchCompanion("cake", { dcAccountId: 2 });
  await ready(h);
  assert.ok(h.rpc.secureJoins.length >= 1);
  assert.match(h.rpc.secureJoins[0]?.qr ?? "", /i\.delta\.chat/);
});

test("pairing and unpairing expose only verified human contacts", async () => {
  const h = harness();
  h.db.upsertCompanion({ id: "greg", name: "Greg", configured: true });
  await h.rpc.addAccount();
  h.db.patchCompanion("greg", { dcAccountId: 1 });
  const sam = h.rpc.addContact(1, { address: "sam@example.org", displayName: "Sam" });
  h.rpc.addContact(1, { address: "spam@example.org", isVerified: false });
  await ready(h);
  const contacts = await h.service.listPairedContacts("greg");
  assert.deepEqual(contacts.map((contact) => contact.name), ["Sam"]);
  const invite = await h.service.pairInvite("greg");
  assert.ok(!invite.includes("n=Name"));
  await h.service.unpairContact("greg", sam.id);
  assert.deepEqual(await h.service.listPairedContacts("greg"), []);
  assert.equal(h.rpc.account(1).contacts.has(sam.id), false);
});

test("stdioDcAdapter maps Delta Chat 2.69 isKeyContact + e2eeAvail to isVerified", async () => {
  const verified = {
    id: 21,
    address: "sam@example.org",
    authName: "Sam",
    displayName: "Sam",
    name: "",
    color: "#112233",
    profileImage: null,
    status: "",
    isBlocked: false,
    isBot: false,
    isKeyContact: true,
    e2eeAvail: true,
    lastSeen: 1,
    wasSeenRecently: true,
    verifierId: 0,
  };
  const noKey = { ...verified, id: 22, address: "nokey@example.org", e2eeAvail: false };
  const addrOnly = { ...verified, id: 23, address: "addr@example.org", isKeyContact: false, e2eeAvail: false };
  const listeners = new Map<string, (accountId: number, event: Record<string, unknown>) => void>();
  const fakeDc = {
    rpc: {
      getContact: async (_accountId: number, contactId: number) => [verified, noKey, addrOnly].find((c) => c.id === contactId)!,
      getContacts: async () => [verified, noKey, addrOnly],
    },
    on(event: string, listener: (accountId: number, event: Record<string, unknown>) => void) {
      listeners.set(event, listener);
    },
    close() {},
  } as unknown as StdioDcSource;
  const rpc = stdioDcAdapter(fakeDc);
  assert.equal((await rpc.getContact(5, 21)).isVerified, true);
  assert.equal((await rpc.getContact(5, 22)).isVerified, false);
  assert.equal((await rpc.getContact(5, 23)).isVerified, false);
  assert.deepEqual(
    (await rpc.getContacts(5, 0, null)).map((c) => ({ id: c.id, isVerified: c.isVerified })),
    [
      { id: 21, isVerified: true },
      { id: 22, isVerified: false },
      { id: 23, isVerified: false },
    ],
  );
  const incoming: Array<[number, number]> = [];
  rpc.subscribe({
    onIncomingMessage: (accountId, msgId) => incoming.push([accountId, msgId]),
    onSecureJoinVerified: () => undefined,
  });
  listeners.get("IncomingMsg")?.(5, { kind: "IncomingMsg", chatId: 17, msgId: 54764 });
  assert.deepEqual(incoming, [[5, 54764]]);
});
