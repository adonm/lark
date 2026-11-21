import {api, request} from "./api.js";
import {$, companionName, node, say} from "./dom.js";
import {state} from "./state.js";

const dialog = node("dialog", "photo-library");
dialog.id = "photo-library";
dialog.setAttribute("aria-labelledby", "photo-library-heading");
dialog.innerHTML = `
  <header>
    <div>
      <p class="eyebrow">On your network / Family photos</p>
      <h2 id="photo-library-heading">Photo Library</h2>
    </div>
    <form method="dialog"><button id="photo-library-close" class="outline" type="submit" autofocus>Close</button></form>
  </header>
  <div class="photo-body">
    <p class="small">Manage which companions can see your library, organize photos, and review people groups.</p>
    <div class="photo-toolbar" role="group" aria-label="Photo access">
      <label data-field for="photo-library-companion">Companion or device <select id="photo-library-companion"></select></label>
      <label class="switch" for="photo-library-allow">
        <input id="photo-library-allow" type="checkbox" role="switch" disabled> Allow photo access for this companion
      </label>
    </div>
    <search>
      <form id="photo-library-search" class="photo-search">
        <label data-field for="photo-library-query">Find photos by name, caption, album or label
          <input id="photo-library-query" type="search" maxlength="160" enterkeyhint="search"
                 autocapitalize="none" autocorrect="off" spellcheck="false" placeholder="Leave blank for recent photos">
        </label>
        <button id="photo-library-find" type="submit" disabled>Find photos</button>
        <button id="photo-library-people" class="outline" type="button" disabled>People</button>
      </form>
    </search>
    <p id="photo-library-status" class="photo-status" role="status" tabindex="-1"></p>
    <ol id="photo-library-results" class="photo-results" role="list"></ol>
  </div>`;
document.body.append(dialog);
const companions = $("photo-library-companion");
const allow = $("photo-library-allow");
const find = $("photo-library-find");
const peopleButton = $("photo-library-people");
const results = $("photo-library-results");
const urls = new Set();
let revision = 0;
let mergeTarget;

function resetResults() {
  revision++;
  for (const url of urls) URL.revokeObjectURL(url);
  urls.clear();
  results.replaceChildren();
}

function setPhotoLibrary(authorized) {
  $("library-toggle").hidden = !authorized;
  if (!authorized && dialog.open) dialog.close();
}

function previewButton(current, path, item, stamp) {
  const preview = node("button", "outline", "Load preview");
  preview.setAttribute("aria-label", `Load preview: ${item.title || "Library face"}`);
  preview.onclick = async () => {
    const focused = document.activeElement === preview;
    preview.disabled = true;
    try {
      await api(current, path, {method:"POST", body:{op:"prepare", handle:item.handle}});
      const face = item.face === undefined ? "" : `?face=${item.face}`;
      const res = await request(current.token, `/api/library/thumbnail/${encodeURIComponent(item.handle)}${face}`);
      if (!res.ok) throw new Error();
      const blob = await res.blob();
      if (revision !== stamp || state.app !== current || !dialog.open) return;
      if (blob.type !== "image/jpeg") throw new Error();
      const url = URL.createObjectURL(blob);
      urls.add(url);
      const img = node("img");
      img.src = url;
      img.alt = item.title || "Library face";
      img.width = item.face === undefined ? 180 : 96;
      preview.replaceWith(img);
      if (focused && document.activeElement === document.body) {
        img.tabIndex = -1;
        img.focus();
      }
    } catch {
      if (revision === stamp) say("photo-library-status", "Could not load this photo.");
      preview.disabled = false;
    }
  };
  return preview;
}

function showResult(current, path, item, stamp) {
  const row = node("li", "card");
  const title = node("h3", "", item.title || "Photo");
  const field = node("label", "", "Labels");
  field.setAttribute("data-field", "");
  const labels = node("input");
  labels.placeholder = "e.g. Grandma, Beach";
  labels.setAttribute("aria-label", `Labels for ${item.title || "photo"}`);
  labels.value = item.tags.join(", ");
  labels.maxLength = 600;
  field.append(labels);
  const save = node("button", "outline", "Save labels");
  save.setAttribute("aria-label", `Save labels: ${item.title || "Photo"}`);
  save.onclick = async () => {
    save.disabled = true;
    try {
      await api(current, path, {method:"POST", body:{
        op:"labels", handle:item.handle, labels:labels.value.split(",").map(s => s.trim()).filter(Boolean)
      }});
      if (revision === stamp) say("photo-library-status", "Labels saved.");
    } catch {
      if (revision === stamp) say("photo-library-status", "Could not save labels.");
    } finally {
      save.disabled = false;
    }
  };
  row.append(title, previewButton(current, path, item, stamp), field, save);
  results.append(row);
}

function listButton(label, action) {
  const button = node("button", "outline", label);
  button.onclick = action;
  return button;
}

async function showPeople(offset = 0, cluster = "") {
  const focusStatus = results.contains(document.activeElement);
  resetResults();
  const current = state.app, stamp = revision;
  if (!current || current.scope || !companions.value || !allow.checked) return;
  const path = `/api/library?device_id=${encodeURIComponent(companions.value)}`;
  const post = (body, again) => async e => {
    const button = e.currentTarget;
    button.disabled = true;
    try {
      await api(current, path, {method:"POST", body:body()});
      if (stamp === revision && state.app === current) again();
    } catch {
      if (stamp === revision) say("photo-library-status", "Could not update this group.");
    } finally {
      button.disabled = false;
    }
  };
  try {
    const data = await api(current, `${path}&people=${offset}${cluster ? `&cluster=${encodeURIComponent(cluster)}` : ""}`);
    if (stamp !== revision || state.app !== current || !dialog.open) return;
    if (!cluster) {
      say("photo-library-status", !data.people.length ?
        data.ml ? "No face groups yet. Groups appear as photos are processed." : "Local photo models are not installed." :
        mergeTarget ? `Merge destination: ${mergeTarget.name || "unnamed group"}. Choose a different group to merge.` :
          "Review and confirm faces before a name can match them.");
      for (const group of data.people) {
        const row = node("li", "card");
        const confirmed = node("span", "badge", `${group.confirmed} of ${group.total} confirmed`);
        confirmed.dataset.variant = "secondary";
        row.append(node("h3", "", group.name || "Unnamed group"), confirmed,
          listButton("Review faces", () => showPeople(0, group.id)),
          listButton("Use as merge destination", () => { mergeTarget = group; showPeople(offset); }));
        if (mergeTarget && mergeTarget.id !== group.id) {
          const merge = post(() => ({op:"person-merge", cluster:group.id, target:mergeTarget.id}), () => showPeople(offset));
          row.append(listButton("Merge into destination", e => {
            if (confirm("Move these faces to the destination group as unconfirmed suggestions?")) merge(e);
          }));
        }
        results.append(row);
      }
    } else {
      say("photo-library-status", "Only confirmed faces are searchable by this name.");
      const edit = node("li", "card photo-wide");
      const field = node("label", "", "Person name");
      field.setAttribute("data-field", "");
      field.htmlFor = "photo-person-name";
      const name = node("input");
      name.id = field.htmlFor;
      name.value = data.name;
      name.placeholder = "e.g. Grandma";
      name.maxLength = 80;
      field.append(name);
      edit.append(listButton("Back to People", () => showPeople()), field,
        listButton("Save name", post(() => ({op:"person-name", cluster, name:name.value}), () => showPeople(offset, cluster))));
      results.append(edit);
      for (const item of data.faces) {
        const row = node("li", "card");
        const badge = node("span", "badge", item.confirmed ? "Confirmed" : "Suggested");
        badge.dataset.variant = item.confirmed ? "success" : "secondary";
        const action = (op, label) => listButton(label, post(() => ({op, handle:item.handle, face:item.face}),
          () => showPeople(0, cluster)));
        row.append(badge, previewButton(current, path, item, stamp));
        if (!item.confirmed) row.append(action("face-confirm", "Confirm match"));
        row.append(action("face-reject", "Reject"), action("face-split", "Split into new group"));
        results.append(row);
      }
    }
    if (offset || data.next) {
      const page = node("li", "row wrap photo-wide");
      if (offset) page.append(listButton("Previous page", () => showPeople(Math.max(0, offset - (cluster ? 6 : 12)), cluster)));
      if (data.next) page.append(listButton("Next page", () => showPeople(data.next, cluster)));
      results.append(page);
    }
  } catch {
    if (stamp === revision) say("photo-library-status", "Photo people unavailable.");
  } finally {
    if (focusStatus && stamp === revision && dialog.open && !dialog.contains(document.activeElement)) {
      $("photo-library-status").focus();
    }
  }
}

async function refresh(search = false) {
  resetResults();
  const current = state.app, stamp = revision;
  allow.disabled = find.disabled = peopleButton.disabled = true;
  if (!search) allow.checked = false;
  if (!current || current.scope || !companions.value) return;
  const path = `/api/library?device_id=${encodeURIComponent(companions.value)}`;
  const q = search ? `&q=${encodeURIComponent($("photo-library-query").value.trim() || "*")}` : "";
  try {
    const data = await api(current, path + q);
    if (revision !== stamp || state.app !== current || !dialog.open) return;
    if (!data.configured) {
      say("photo-library-status", "Set up Ente Photos on the bridge host first.");
      return;
    }
    allow.checked = !data.hidden;
    allow.disabled = false;
    find.disabled = !data.ready || data.hidden;
    peopleButton.disabled = find.disabled;
    say("photo-library-status", !data.ready ? "Waiting for fresh Ente metadata." :
      data.hidden ? "The library is hidden from this companion." :
      search ? data.items.length ? `${data.items.length} photo${data.items.length === 1 ? "" : "s"} found.` : "No matching photos." :
      "Search for photos or open People to review face groups.");
    for (const item of data.items) showResult(current, path, item, stamp);
  } catch {
    if (revision === stamp) say("photo-library-status", "Photo library unavailable.");
  }
}

$("library-toggle").onclick = () => {
  const current = state.app;
  if (!current || current.scope || dialog.open) return;
  companions.replaceChildren(...current.devices.map(d => {
    const option = node("option", "", companionName(d) || d.device_id);
    option.value = d.device_id;
    return option;
  }));
  dialog.showModal();
  say("photo-library-status", companions.value ? "" : "No companions found.");
  refresh();
};
dialog.onclose = () => { resetResults(); mergeTarget = null; };
companions.onchange = () => { mergeTarget = null; refresh(); };
$("photo-library-people").onclick = () => showPeople();
$("photo-library-search").onsubmit = e => { e.preventDefault(); refresh(true); };
allow.onchange = async () => {
  const current = state.app;
  if (!current || current.scope || !companions.value) return;
  allow.disabled = find.disabled = peopleButton.disabled = true;
  resetResults();
  const stamp = revision;
  try {
    await api(current, `/api/library?device_id=${encodeURIComponent(companions.value)}`,
      {method:"POST", body:{op:"visibility", hidden:!allow.checked}});
    if (stamp === revision) await refresh();
  } catch {
    if (stamp === revision) {
      allow.checked = !allow.checked;
      allow.disabled = false;
      say("photo-library-status", "Could not change photo access.");
    }
  }
};

export {setPhotoLibrary};
