// Delta Chat text commands and the welcome message. Commands are stateless
// apart from the shared database and optional remote-control hooks.
import type { Companion, LarkDb } from "./db.ts";
import type { DcContact, DcRpc } from "./deltachat.ts";
import type { RemoteManager } from "./remote.ts";

export function normalizeAddress(address: string): string {
  return address.trim().toLowerCase();
}

export function contactKey(contact: DcContact): string {
  const address = normalizeAddress(contact.address);
  return address === "" ? String(contact.id) : address;
}

export function contactName(contact: DcContact): string {
  const display = contact.displayName.trim();
  if (display !== "") return display;
  const name = contact.name.trim();
  if (name !== "") return name;
  return contact.address.trim();
}

export function welcomeText(companion: Pick<Companion, "name" | "form">): string {
  const name = companion.name.trim() === "" ? "Lark" : companion.name.trim();
  const form = companion.form.trim();
  const intro = form === "" ? `Hi! I'm ${name}.` : `Hi! I'm ${name}, ${form}.`;
  return [
    `${intro} Text me anytime — I'll read it out at home and reply.`,
    "/say <words> — I'll say it out loud exactly",
    "/private <words> — just between us",
    "/stories — list interactive stories",
    "/story <name> — open a story on my device",
    "/remote — open my remote controls",
    "/remote off — turn off my remote link",
    "/help — this message",
    "Send a .twee file to import a story. When listening mode is on, people at home can answer you out loud.",
  ].join("\n");
}

export function parseSlashCommand(text: string): { name: string; arg: string } {
  const trimmed = text.trim();
  if (!trimmed.startsWith("/")) return { name: "", arg: trimmed };
  const space = trimmed.search(/[ \t\n\r]/);
  if (space < 0) return { name: trimmed.toLowerCase(), arg: "" };
  return { name: trimmed.slice(0, space).toLowerCase(), arg: trimmed.slice(space).trim() };
}

export interface DcCommandContext {
  db: LarkDb;
  remote?: RemoteManager;
  onSay?: (companionId: string, text: string) => Promise<void>;
  onStory?: (companionId: string, storyId: string) => Promise<void>;
}

export interface DcCommandOutcome {
  handled: boolean;
  private: boolean;
  text: string;
}

export async function runDcCommand(
  ctx: DcCommandContext,
  rpc: DcRpc,
  accountId: number,
  chatId: number,
  companion: Companion,
  contact: DcContact,
  command: { name: string; arg: string },
): Promise<DcCommandOutcome> {
  const say = async (text: string): Promise<void> => {
    await rpc.sendMsg(accountId, chatId, { text });
  };
  switch (command.name) {
    case "/say":
      if (command.arg === "") {
        await say("Usage: /say <words>");
        return { handled: true, private: false, text: "" };
      }
      await ctx.onSay?.(companion.id, command.arg);
      await say("Said it.");
      return { handled: true, private: false, text: "" };
    case "/private":
      if (command.arg === "") {
        await say("Usage: /private <words>");
        return { handled: true, private: false, text: "" };
      }
      return { handled: false, private: true, text: command.arg };
    case "/help":
      await say(welcomeText(companion));
      return { handled: true, private: false, text: "" };
    case "/remote": {
      const remote = ctx.remote;
      if (remote === undefined) {
        await say("Remote controls unavailable.");
        return { handled: true, private: false, text: "" };
      }
      const key = contactKey(contact);
      if (command.arg.toLowerCase() === "off") {
        await remote.revokeContact(companion.id, key);
        await say("Remote controls off.");
        return { handled: true, private: false, text: "" };
      }
      if (command.arg !== "") {
        await say("Usage: /remote [off]");
        return { handled: true, private: false, text: "" };
      }
      try {
        const link = await remote.issue(companion.id, key, contactName(contact));
        await say(link);
      } catch (error) {
        await say(error instanceof Error ? error.message : "Remote controls unavailable.");
      }
      return { handled: true, private: false, text: "" };
    }
    case "/stories": {
      const stories = ctx.db.listStories();
      if (stories.length === 0) {
        await say("No stories yet.");
      } else {
        await say(
          ["Stories:", ...stories.map((s) => `- ${s.title} (${s.id}, ${s.passages} passages)`)].join("\n"),
        );
      }
      return { handled: true, private: false, text: "" };
    }
    case "/story": {
      if (command.arg === "") {
        await say("Usage: /story <name>");
        return { handled: true, private: false, text: "" };
      }
      const target = command.arg.toLowerCase();
      const story = ctx.db
        .listStories()
        .find((s) => s.id.toLowerCase() === target || s.title.toLowerCase() === target);
      if (story === undefined) {
        await say(`I couldn't find a story called “${command.arg}”.`);
        return { handled: true, private: false, text: "" };
      }
      await ctx.onStory?.(companion.id, story.id);
      await say(`Opening “${story.title}”.`);
      return { handled: true, private: false, text: "" };
    }
    default:
      if (command.name === "") return { handled: false, private: false, text: command.arg };
      await say("Unknown command. Try /say <words>, /private <words>, /stories, /story <name>, /remote, or /help.");
      return { handled: true, private: false, text: "" };
  }
}
