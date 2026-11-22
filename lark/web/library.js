import {api} from "./api.js";
import {$, node, say} from "./dom.js";
import {state} from "./state.js";

function tickTimers() {
  for (const clock of $("timer-items").querySelectorAll("time")) {
    const seconds = Math.max(0, Math.ceil((Date.parse(clock.dateTime) - Date.now()) / 1000));
    clock.textContent = `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
  }
}

function removeButton(text, label, action) {
  const button = node("button", "outline compact-action", text);
  button.type = "button";
  button.setAttribute("aria-label", `${text} ${label}`);
  button.onclick = async () => {
    button.disabled = true;
    try { await action(); } finally { button.disabled = false; }
  };
  return button;
}

async function loadTimers() {
  const current = state.app, view = state.view;
  if (!current || !view?.id) return;
  const request = view.timerRequest = (view.timerRequest || 0) + 1;
  try {
    const {timers} = await api(current, `/api/timers?companion_id=${encodeURIComponent(view.id)}`);
    if (state.app !== current || state.view !== view || view.timerRequest !== request) return;
    const items = timers.map(timer => {
      const item = node("li", "compact-item"), clock = node("time", "timer-clock");
      clock.dateTime = timer.fire_at;
      item.append(clock, node("span", "compact-label", timer.label), removeButton("Cancel", timer.label, async () => {
        try {
          await api(current, `/api/timers/${encodeURIComponent(timer.id)}`, {method:"DELETE"});
          if (state.view === view) say("timer-note", "");
        } catch {
          if (state.view === view) say("timer-note", "The timer could not be canceled. It may have already fired.");
        }
        if (state.view === view) loadTimers();
      }));
      return item;
    });
    $("timer-items").replaceChildren(...items);
    $("active-timers").hidden = !items.length;
    tickTimers();
  } catch {
    if (state.view === view && view.timerRequest === request) say("timer-note", "Could not load timers.");
  }
}

async function loadSaved() {
  const current = state.app, view = state.view, details = $("saved-state");
  if (!current || current.scope || !details.open) return;
  const request = current.savedRequest = (current.savedRequest || 0) + 1;
  const active = () => state.app === current && state.view === view && current.savedRequest === request;
  say("saved-note", "Loading…");
  $("saved-items").replaceChildren();
  try {
    const [{scopes}, {apps}] = await Promise.all([api(current, "/api/memory/scopes"), api(current, "/api/apps")]);
    if (!active()) return;
    const names = [...new Set(["shared", ...(view?.id ? [view.id] : []), ...scopes.map(s => s.scope).filter(s => /^(game|app):/.test(s))])];
    const memory = await Promise.all(names.map(scope => api(current, `/api/memory?scope=${encodeURIComponent(scope)}`)));
    if (!active()) return;
    const remove = (path, label) => removeButton("Delete", label, async () => {
      try {
        await api(current, path, {method:"DELETE"});
        if (state.app === current) loadSaved();
      } catch {
        if (state.app === current) say("saved-note", "Could not delete the saved item.");
      }
    });
    const section = (label, entries) => {
      const group = node("section", "saved-group"), list = node("ul", "compact-list");
      list.append(...entries);
      group.append(node("h3", "", label), entries.length ? list : node("p", "small", "Nothing saved."));
      return group;
    };
    const groups = memory.map(({scope, entries}) => section(scope, entries.map(entry => {
      const item = node("li"), row = node("div", "compact-item");
      row.append(node("code", "compact-label", entry.key), remove(
        `/api/memory?scope=${encodeURIComponent(scope)}&key=${encodeURIComponent(entry.key)}`, entry.key));
      item.append(row, node("pre", "saved-value", JSON.stringify(entry.value, null, 2)));
      return item;
    })));
    groups.push(section("Saved Apps", apps.map(saved => {
      const item = node("li", "compact-item");
      const run = node("button", "small", "Run");
      run.type = "button";
      run.disabled = !view?.id;
      run.setAttribute("aria-label", `Run ${saved.title}`);
      run.onclick = async () => {
        run.disabled = true;
        try {
          await api(current, "/api/apps/run", {method:"POST", body:{companion_id:view.id,name:saved.name}});
          if (active()) say("saved-note", `Running ${saved.title}.`);
        } catch (error) {
          if (active()) say("saved-note", error.message || "Could not run the saved app.");
        } finally {
          run.disabled = false;
        }
      };
      item.append(node("span", "compact-label", `${saved.title} · ${saved.kind}`), run,
        remove(`/api/apps/${encodeURIComponent(saved.name)}`, saved.title));
      return item;
    })));
    $("saved-items").replaceChildren(...groups);
    say("saved-note", "");
  } catch {
    if (active()) say("saved-note", "Could not load saved items.");
  }
}
$("saved-state").ontoggle = loadSaved;

export {loadSaved, loadTimers, tickTimers};
