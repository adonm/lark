#!/usr/bin/env node
// Minimal Delta Chat JSON-RPC stub for the cross-process end-to-end test.
// It speaks the same newline-delimited JSON-RPC as deltachat-rpc-server, keeps
// in-memory accounts, and exposes an HTTP injector so the test can simulate an
// incoming message without a relay account.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { join } from "node:path";
import { createInterface } from "node:readline";

const accounts = new Map();
let nextAccount = 0;
let nextMessage = 100;
let nextContact = 20;
const events = [];
const pending = [];
const sent = [];

// deltachat-rpc-server keeps accounts on disk; the stub keeps the same
// account ids and config across a brain restart so the companion link survives.
const accountsDir = process.env.DC_ACCOUNTS_PATH ?? ".";
const accountsFile = join(accountsDir, "stub-accounts.json");
function persist() {
  mkdirSync(accountsDir, { recursive: true });
  writeFileSync(
    accountsFile,
    JSON.stringify({
      nextAccount,
      accounts: [...accounts.values()].map((account) => ({
        id: account.id,
        address: account.address,
        config: [...account.config.entries()],
        contacts: account.contacts,
      })),
    }),
  );
}
if (existsSync(accountsFile)) {
  try {
    const saved = JSON.parse(readFileSync(accountsFile, "utf8"));
    nextAccount = saved.nextAccount ?? 0;
    for (const entry of saved.accounts ?? []) {
      accounts.set(entry.id, {
        id: entry.id,
        address: entry.address,
        config: new Map(entry.config ?? []),
        contacts: entry.contacts?.length > 0 ? entry.contacts : contacts.map((contact) => ({ ...contact })),
        messages: new Map(),
        chats: new Map(),
      });
    }
  } catch {
    // Start empty when the fixture file is incomplete.
  }
}

const contacts = [
  { id: 7, address: "sam@example.org", displayName: "Sam", name: "Sam", isBot: false, isBlocked: false, isKeyContact: true, e2eeAvail: true },
  { id: 8, address: "alex@example.org", displayName: "Alex", name: "Alex", isBot: false, isBlocked: false, isKeyContact: true, e2eeAvail: true },
];

function account(id) {
  const value = accounts.get(id);
  if (value === undefined) throw new Error(`no account ${id}`);
  return value;
}

function addAccount() {
  const id = ++nextAccount;
  const value = {
    id,
    address: `companion${id}@nine.testrun.org`,
    config: new Map(),
    contacts: contacts.map((contact) => ({ ...contact })),
    messages: new Map(),
    chats: new Map(),
  };
  value.config.set("addr", value.address);
  accounts.set(id, value);
  persist();
  return id;
}

function flush() {
  while (pending.length > 0 && events.length > 0) {
    const id = pending.shift();
    const event = events.shift();
    write({ jsonrpc: "2.0", id, result: [event] });
  }
  while (pending.length > 0) {
    const id = pending.shift();
    write({ jsonrpc: "2.0", id, result: [] });
  }
}

function write(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function ok(id, result) {
  write({ jsonrpc: "2.0", id, result });
}

function fail(id, message) {
  write({ jsonrpc: "2.0", id, error: { code: -1, message } });
}

function inject({ sender, address, text, viewType = "Text", file = null }) {
  const list = [...accounts.values()];
  if (list.length === 0) throw new Error("no account provisioned yet");
  const acc = list[0];
  let contact = acc.contacts.find((entry) => entry.displayName === sender || entry.address === address);
  if (contact === undefined) {
    contact = {
      id: nextContact++,
      address: address ?? `${sender?.toLowerCase() ?? "friend"}@example.org`,
      displayName: sender ?? "Friend",
      name: sender ?? "Friend",
      isBot: false,
      isBlocked: false,
      isKeyContact: true,
      e2eeAvail: true,
    };
    acc.contacts.push(contact);
  }
  const id = ++nextMessage;
  const chatId = 100 + contact.id;
  acc.messages.set(id, {
    id,
    chatId,
    fromId: contact.id,
    text: text ?? "",
    viewType,
    file,
    isInfo: false,
    isBot: false,
  });
  acc.chats.set(chatId, { id: chatId, name: contact.displayName, isContactRequest: false });
  events.push({ contextId: acc.id, event: { kind: "IncomingMsg", chatId, msgId: id } });
  flush();
  return { accountId: acc.id, messageId: id, chatId, contactId: contact.id };
}

const httpPort = Number.parseInt(process.env.LARK_DC_STUB_PORT ?? "0", 10);
const server = createServer(async (request, response) => {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  const body = chunks.length === 0 ? {} : JSON.parse(Buffer.concat(chunks).toString("utf8"));
  const send = (status, value) => {
    response.writeHead(status, { "content-type": "application/json" });
    response.end(JSON.stringify(value));
  };
  if (request.url === "/state") {
    send(200, {
      accounts: [...accounts.values()].map((acc) => ({ id: acc.id, address: acc.address, contacts: acc.contacts.length })),
      sent: sent.length,
    });
    return;
  }
  if (request.url === "/sent") {
    send(200, { sent });
    return;
  }
  if (request.url === "/inject" && request.method === "POST") {
    try {
      send(200, inject(body));
    } catch (error) {
      send(500, { error: String(error) });
    }
    return;
  }
  send(404, { error: "Not found" });
});
server.listen(httpPort, "127.0.0.1", () => {
  process.stderr.write(`stub-dc: injector on ${JSON.stringify(server.address())}\n`);
});

async function handle(request) {
  const method = request.method;
  const args = request.params ?? [];
  switch (method) {
    case "add_account": {
      const id = addAccount();
      ok(request.id, id);
      return;
    }
    case "remove_account": {
      accounts.delete(args[0]);
      persist();
      ok(request.id, null);
      return;
    }
    case "get_all_account_ids":
      ok(request.id, [...accounts.keys()].sort((a, b) => a - b));
      return;
    case "start_io_for_all_accounts":
      ok(request.id, null);
      return;
    case "add_transport_from_qr":
      account(args[0]).config.set("transport", args[1]);
      persist();
      ok(request.id, null);
      return;
    case "batch_set_config": {
      const acc = account(args[0]);
      for (const [key, value] of Object.entries(args[1] ?? {})) if (value !== null) acc.config.set(key, value);
      persist();
      ok(request.id, null);
      return;
    }
    case "set_config": {
      const acc = account(args[0]);
      if (args[2] === null) acc.config.delete(args[1]);
      else acc.config.set(args[1], args[2]);
      persist();
      ok(request.id, null);
      return;
    }
    case "get_config": {
      const value = account(args[0]).config.get(args[1]);
      ok(request.id, value ?? null);
      return;
    }
    case "create_qr_svg":
      ok(request.id, `<svg>${args[0]}</svg>`);
      return;
    case "get_chat_securejoin_qr_code":
      ok(request.id, `https://i.delta.chat/#stub&a=${encodeURIComponent(account(args[0]).address)}`);
      return;
    case "secure_join": {
      const acc = account(args[0]);
      const contact = acc.contacts[0] ?? contacts[0];
      const chatId = 100 + contact.id;
      acc.chats.set(chatId, { id: chatId, name: contact.displayName, isContactRequest: false });
      ok(request.id, chatId);
      return;
    }
    case "get_contacts":
      ok(request.id, account(args[0]).contacts);
      return;
    case "get_contact": {
      const contact = account(args[0]).contacts.find((entry) => entry.id === args[1]);
      if (contact === undefined) fail(request.id, "contact not found");
      else ok(request.id, contact);
      return;
    }
    case "delete_contact":
      account(args[0]).contacts = account(args[0]).contacts.filter((entry) => entry.id !== args[1]);
      ok(request.id, null);
      return;
    case "get_chat_id_by_contact_id": {
      const acc = account(args[0]);
      const contact = acc.contacts.find((entry) => entry.id === args[1]);
      const chat = contact === undefined ? undefined : [...acc.chats.values()].find((entry) => entry.name === contact.displayName);
      ok(request.id, chat?.id ?? null);
      return;
    }
    case "create_chat_by_contact_id": {
      const acc = account(args[0]);
      const contact = acc.contacts.find((entry) => entry.id === args[1]);
      const chatId = 1000 + args[1];
      acc.chats.set(chatId, { id: chatId, name: contact?.displayName ?? "Chat", isContactRequest: false });
      ok(request.id, chatId);
      return;
    }
    case "delete_chat":
      account(args[0]).chats.delete(args[1]);
      ok(request.id, null);
      return;
    case "get_basic_chat_info": {
      const acc = account(args[0]);
      const chat = acc.chats.get(args[1]) ?? { id: args[1], name: "Chat", isContactRequest: false };
      ok(request.id, chat);
      return;
    }
    case "get_message": {
      const message = account(args[0]).messages.get(args[1]);
      if (message === undefined) fail(request.id, "message not found");
      else ok(request.id, message);
      return;
    }
    case "send_msg": {
      const data = args[2] ?? {};
      sent.push({ accountId: args[0], chatId: args[1], text: data.text ?? "", data });
      ok(request.id, ++nextMessage);
      return;
    }
    case "get_next_event_batch": {
      if (events.length > 0) {
        ok(request.id, [events.shift()]);
      } else {
        pending.push(request.id);
        setTimeout(flush, 20);
      }
      return;
    }
    default:
      ok(request.id, null);
  }
}

const lines = createInterface({ input: process.stdin });
lines.on("line", (line) => {
  if (line.trim() === "") return;
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    return;
  }
  if (typeof request.id !== "number") return;
  Promise.resolve(handle(request)).catch((error) => fail(request.id, String(error)));
});
lines.on("close", () => {
  server.close();
  process.exit(0);
});
