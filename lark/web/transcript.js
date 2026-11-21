// Records are ordered like the API cursor: millisecond timestamp, then ID.
export function transcript(list, makeBubble, makeDay, dayLabel) {
  const records = [], byID = new Map();
  const compare = (a, b) => a.ts - b.ts || (a.entry.id < b.entry.id ? -1 : a.entry.id > b.entry.id ? 1 : 0);
  function lowerBound(before) {
    let lo = 0, hi = records.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (before(records[mid])) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }
  function separator(i) {
    const cur = records[i];
    if (!cur) return;
    if (cur.day && cur.day !== records[i - 1]?.day) {
      if (!cur.heading) {
        cur.heading = makeDay(cur.day);
        list.insertBefore(cur.heading, cur.node);
      }
    } else if (cur.heading) {
      cur.heading.remove();
      cur.heading = null;
    }
  }
  function upsert(entry) {
    if (!entry.id) return;
    const when = new Date(entry.timestamp);
    const valid = Number.isFinite(when.valueOf());
    const ts = valid ? when.valueOf() : -Infinity, day = valid ? dayLabel(when) : "";
    const previous = byID.get(entry.id);
    if (previous?.ts === ts) {
      if (["role", "sender", "recipient", "source", "text"].every(key => previous.entry[key] === entry[key])) return;
      const updated = makeBubble(entry);
      previous.node.replaceWith(updated);
      previous.node = updated;
      previous.entry = entry;
      return;
    }
    if (previous) {
      const i = lowerBound(p => compare(p, previous) < 0);
      records.splice(i, 1);
      previous.heading?.remove();
      previous.node.remove();
      separator(i);
    }
    const item = {entry, ts, day, node:makeBubble(entry), heading:null};
    const i = lowerBound(p => compare(p, item) < 0), next = records[i];
    list.insertBefore(item.node, next?.heading || next?.node || null);
    records.splice(i, 0, item);
    byID.set(entry.id, item);
    separator(i);
    separator(i + 1);
  }
  return {
    get size() { return records.length; },
    node: id => byID.get(id)?.node,
    anchor: top => records[lowerBound(({node}) => node.offsetTop + node.offsetHeight <= top)],
    clear() {
      list.replaceChildren();
      records.length = 0;
      byID.clear();
    },
    upsert,
  };
}
