import {api} from "./api.js";
import {unlockAudio} from "./audio.js";
import {$, node, say} from "./dom.js";
import {renderCurrent} from "./live.js";
import {state} from "./state.js";
import {transcript} from "./transcript.js";

const dayFormat = new Intl.DateTimeFormat(undefined, {dateStyle:"medium"});
const timeFormat = new Intl.DateTimeFormat(undefined, {hour:"2-digit", minute:"2-digit", hourCycle:"h23"});
function bubble(entry) {
  if (entry.role === "card" || entry.role === "system") {
    const item = node("li", "badge outline chat-divider", entry.text);
    item.dataset.entry = entry.id;
    return item;
  }
  const role = ["user", "intercom"].includes(entry.role) ? entry.role : "companion";
  const item = node("li", `bubble ${role}`);
  item.dataset.entry = entry.id;
  const name = state.view.companion?.name || "Companion";
  const sender = entry.sender || (role === "user" ? "You" : name);
  const recipient = entry.recipient || (sender !== name ? name : "");
  const label = role === "intercom" ? recipient ? `${sender} → ${recipient}` : `From ${sender}` : sender;
  const header = node("div", "row wrap bubble-head");
  header.append(node("strong", "", label));
  if (role === "intercom") header.append(node("span", "badge outline", "Intercom"));
  else if (role === "user") header.append(node("span", "badge outline", {voice:"Voice", tap:"Tap"}[entry.source] || "Text"));
  const when = new Date(entry.timestamp);
  if (Number.isFinite(when.valueOf())) {
    const time = node("time", "", timeFormat.format(when));
    time.dateTime = when.toISOString();
    header.append(time);
  }
  item.append(header, node("p", "bubble-text", entry.text));
  return item;
}

const chat = transcript($("messages"), bubble, day => node("li", "day", day), day => dayFormat.format(day));
const transcriptPane = $("transcript");
const nearLatest = () => transcriptPane.scrollHeight - transcriptPane.clientHeight - transcriptPane.scrollTop <= 80;
let following = true;
transcriptPane.addEventListener("scroll", () => { following = nearLatest(); });
new ResizeObserver(() => {
  if (following) transcriptPane.scrollTop = transcriptPane.scrollHeight;
}).observe(transcriptPane);

function transcriptStatus() {
  $("empty-chat").hidden = chat.size !== 0;
  say("empty-chat", !state.view.id ? "Choose a companion to open its chat." : state.view.loading ? "Loading chat…" :
    state.view.historyError ? "Could not load chat. Try Refresh." : "No messages yet — say hi or tap the mic!");
  $("load-older").hidden = !state.view.cursor;
  $("load-older").disabled = !!state.view.loading || !!state.view.resetting;
  say("load-older", state.view.olderLoading ? "Loading older…" : state.view.olderError ? "Retry loading older" : "Load older");
}

function updateTranscript(update, keepPosition = false) {
  const scroll = transcriptPane, top = scroll.scrollTop;
  const follow = !keepPosition && nearLatest();
  const anchor = follow ? null : chat.anchor(top);
  const offset = anchor?.node.offsetTop;
  const id = anchor?.entry.id;
  update();
  transcriptStatus();
  if (follow) scroll.scrollTop = scroll.scrollHeight;
  else {
    const replacement = id && chat.node(id);
    scroll.scrollTop = replacement ? top + replacement.offsetTop - offset : top;
  }
  following = nearLatest();
}

function resetTranscript() {
  chat.clear();
  transcriptPane.scrollTop = 0;
  following = true;
  transcriptStatus();
}

function cancelHistoryLoad(cat) {
  cat.loading?.abort();
  cat.loading = cat.liveDuringLoad = null;
  cat.olderLoading = false;
}

function reloadArchivedHistory() {
  const cat = state.view;
  cancelHistoryLoad(cat);
  cat.cursor = cat.latestPageID = null;
  cat.olderError = cat.historyError = false;
  resetTranscript();
  loadHistory();
}

function receiveChat(entry) {
  if (!entry?.id) return;
  state.view.liveDuringLoad?.set(entry.id, entry);
  updateTranscript(() => chat.upsert(entry));
}

async function loadHistory({replace = true, older = false} = {}) {
  const cat = state.view, current = state.app;
  if (!cat?.id || cat.resetting || older && (cat.loading || !cat.cursor)) return;
  cat.loading?.abort();
  const loading = new AbortController(), live = new Map();
  cat.loading = loading;
  cat.liveDuringLoad = live;
  cat.olderLoading = older;
  cat.olderError = false;
  cat.historyError = false;
  renderCurrent();
  transcriptStatus();
  try {
    const query = older ? `?${new URLSearchParams(cat.cursor)}` : "";
    const page = await api(current, `/api/companions/${encodeURIComponent(cat.id)}/messages${query}`, {signal:loading.signal});
    if (state.view !== cat || cat.loading !== loading) return;
    // A reconnect may have missed more than a page or a history archive.
    // If the last known message is gone, rebuild from the new page.
    const gap = !older && !!cat.latestPageID &&
      !page.messages.some(entry => entry.id === cat.latestPageID);
    const replacePage = !older && (replace || gap);
    updateTranscript(() => {
      if (replacePage) chat.clear();
      if (older || replacePage || cat.cursor === undefined) cat.cursor = page.has_more ? page.next_cursor : null;
      if (!older) cat.latestPageID = page.messages.at(-1)?.id;
      for (const entry of page.messages) if (!live.has(entry.id)) chat.upsert(entry);
      for (const entry of live.values()) chat.upsert(entry);
    }, older);
  } catch {
    if (!loading.signal.aborted) {
      if (older) cat.olderError = true;
      else cat.historyError = true;
    }
  } finally {
    if (state.view === cat && cat.loading === loading) {
      cat.loading = cat.liveDuringLoad = null;
      cat.olderLoading = false;
      renderCurrent();
      transcriptStatus();
    }
  }
}

$("refresh").onclick = () => loadHistory();
$("load-older").onclick = () => loadHistory({older:true});
$("reset").onclick = async () => {
  const cat = state.view, current = state.app;
  if (!current || current.scope || !cat?.id) return;
  if (!confirm("Start a fresh companion and chat session? The previous chat will be archived, not deleted.")) return;
  cancelHistoryLoad(cat);
  cat.resetting = true;
  renderCurrent();
  transcriptStatus();
  say("chat-note", "Starting a new chat…");
  try {
    await api(current, `/api/companions/${encodeURIComponent(cat.id)}/reset`, {method:"POST", body:{archive_history:true}});
    if (state.view !== cat) return;
    cat.resetting = false;
    say("chat-note", "New chat started. Previous chat archived.");
    reloadArchivedHistory();
  } catch {
    if (state.view === cat) say("chat-note", "Could not start a new chat.");
  } finally {
    cat.resetting = false;
    if (state.view === cat) {
      renderCurrent();
      transcriptStatus();
    }
  }
};

$("send").addEventListener("pointerdown", e => {
  if (e.button === 0 && document.activeElement === $("text")) e.preventDefault();
});
$("prompt").onsubmit = async e => {
  e.preventDefault();
  const cat = state.view, current = state.app, input = $("text"), text = input.value.trim();
  if (!text || !cat?.id || $("send").disabled) return;
  if (cat.web) unlockAudio();
  cat.sending = true;
  renderCurrent();
  say("chat-note", "Sending…");
  try {
    const result = await api(current, "/api/chat", {method:"POST", body:{companion_id:cat.id, text}});
    if (state.view !== cat) return;
    if (input.value.trim() === text) input.value = "";
    say("chat-note", "");
    if (!state.rev || result.reply) loadHistory({replace:false});
  } catch {
    if (state.view === cat) say("chat-note", "Could not send. Your message is still in the composer.");
  } finally {
    cat.sending = false;
    if (state.view === cat) renderCurrent();
  }
};

export {loadHistory, receiveChat, reloadArchivedHistory, resetTranscript};
