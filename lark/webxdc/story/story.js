(function (root) {
  "use strict";

  function containsHTML(s) {
    return /(?:^|[^<])<(?!<)[a-zA-Z/!]/.test(s);
  }

  function parseHeader(line, lineNo) {
    const rest = line.slice(2).trim();
    if (!rest) throw new Error(`line ${lineNo}: empty passage name`);
    const brace = rest.indexOf("{");
    let name = rest;
    let bg = "";
    let end = false;
    if (brace >= 0) {
      name = rest.slice(0, brace).trim();
      let meta;
      try { meta = JSON.parse(rest.slice(brace)); } catch (e) { throw new Error(`line ${lineNo}: invalid metadata JSON`); }
      for (const [k, v] of Object.entries(meta)) {
        if (k === "bg") {
          if (typeof v !== "string") throw new Error(`line ${lineNo}: bg must be a string`);
          bg = v.trim();
        } else if (k === "end") {
          if (typeof v !== "boolean") throw new Error(`line ${lineNo}: end must be a boolean`);
          end = v;
        } else {
          throw new Error(`line ${lineNo}: unsupported metadata key ${k}`);
        }
      }
    }
    if (!name || /[\[\]]/.test(name)) throw new Error(`line ${lineNo}: invalid passage name`);
    return { name, bg, end };
  }

  function parseTwee(src) {
    const lines = src.replace(/\r\n/g, "\n").split("\n");
    const raw = [];
    let cur = null;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i], ln = i + 1;
      if (line.startsWith("::")) {
        if (line.length < 3 || line[2] !== " ") throw new Error(`line ${ln}: passage header must start with ":: "`);
        cur = { ...parseHeader(line, ln), lineNo: ln, lines: [] };
        raw.push(cur);
      } else if (!cur) {
        if (line.trim()) throw new Error(`line ${ln}: content outside passage`);
      } else {
        cur.lines.push({ ln, text: line });
      }
    }
    let title = "", startName = "";
    const storyRaw = [], seen = new Set();
    for (const rp of raw) {
      if (seen.has(rp.name)) throw new Error(`line ${rp.lineNo}: duplicate passage ${rp.name}`);
      seen.add(rp.name);
      const body = rp.lines.map((l) => l.text).join("\n").trim();
      if (rp.name === "StoryTitle") {
        if (!body) throw new Error(`line ${rp.lineNo}: empty StoryTitle`);
        title = body;
      } else if (rp.name === "StoryData") {
        if (body) {
          const d = JSON.parse(body);
          if (typeof d.start === "string") startName = d.start.trim();
        }
      } else if (["StoryIncludes", "StoryStylesheet", "StoryScript"].includes(rp.name)) {
        throw new Error(`line ${rp.lineNo}: unsupported special passage ${rp.name}`);
      } else {
        storyRaw.push(rp);
      }
    }
    if (!title) throw new Error("missing StoryTitle passage");
    if (!storyRaw.length) throw new Error("story has no passages");

    const byName = new Map(storyRaw.map((p, i) => [p.name, i]));
    const start = startName ? byName.get(startName) : (byName.get("Start") ?? 0);
    if (start === undefined) throw new Error(`StoryData start passage ${startName} does not exist`);

    const passages = storyRaw.map((rp) => {
      const ops = [], stack = [];
      let links = 0, textLen = 0;
      for (const { ln, text: line } of rp.lines) {
        if (containsHTML(line)) throw new Error(`line ${ln}: HTML tags are not allowed`);
        let pos = 0, emitted = false;
        while (pos < line.length) {
          const nm = line.indexOf("<<", pos), nl = line.indexOf("[[", pos);
          let idx = -1, kind = 0;
          if (nm >= 0 && (nl < 0 || nm < nl)) { idx = nm; kind = 1; }
          else if (nl >= 0) { idx = nl; kind = 2; }
          if (idx < 0) {
            const chunk = line.slice(pos);
            if (chunk) { ops.push({ op: "text", text: chunk }); textLen += chunk.length; emitted = true; }
            break;
          }
          if (idx > pos) {
            const chunk = line.slice(pos, idx);
            ops.push({ op: "text", text: chunk }); textLen += chunk.length; emitted = true;
          }
          if (kind === 1) {
            const end = line.indexOf(">>", idx + 2);
            if (end < 0) throw new Error(`line ${ln}: unclosed macro <<`);
            const body = line.slice(idx + 2, end).trim();
            pos = end + 2;
            if (body.startsWith("set ")) {
              const m = body.slice(4).trim().match(/^\$([a-zA-Z_][a-zA-Z0-9_]*)\s+to\s+(.+)$/);
              if (!m) throw new Error(`line ${ln}: invalid <<set>>`);
              const rawVal = m[2].trim();
              let val = rawVal === "true" ? true : rawVal === "false" ? false : Number(rawVal);
              if (typeof val === "number" && Number.isNaN(val)) throw new Error(`line ${ln}: invalid <<set>> value`);
              ops.push({ op: "set", flag: m[1], val });
            } else if (body.startsWith("if ")) {
              const m = body.slice(3).trim().match(/^\$([a-zA-Z_][a-zA-Z0-9_]*)$/);
              if (!m) throw new Error(`line ${ln}: invalid <<if>>`);
              stack.push({ ifNot: ops.length, jump: -1, hasElse: false, ln });
              ops.push({ op: "ifnot", flag: m[1], target: -1 });
            } else if (body === "else") {
              if (!stack.length || stack[stack.length - 1].hasElse) throw new Error(`line ${ln}: unexpected <<else>>`);
              const top = stack[stack.length - 1];
              top.hasElse = true;
              top.jump = ops.length;
              ops.push({ op: "jump", target: -1 });
              ops[top.ifNot].target = ops.length;
            } else if (body === "endif") {
              if (!stack.length) throw new Error(`line ${ln}: unexpected <<endif>>`);
              const top = stack.pop();
              ops[top.hasElse ? top.jump : top.ifNot].target = ops.length;
            } else {
              throw new Error(`line ${ln}: unsupported macro <<${body}>>`);
            }
          } else {
            const end = line.indexOf("]]", idx + 2);
            if (end < 0) throw new Error(`line ${ln}: unclosed link [[`);
            const body = line.slice(idx + 2, end).trim();
            pos = end + 2;
            const arrow = body.indexOf("->"), pipe = body.indexOf("|");
            const label = arrow >= 0 ? body.slice(0, arrow).trim() : pipe >= 0 ? body.slice(0, pipe).trim() : body;
            const targetName = arrow >= 0 ? body.slice(arrow + 2).trim() : pipe >= 0 ? body.slice(pipe + 1).trim() : body;
            const target = byName.get(targetName);
            if (!label || target === undefined) throw new Error(`line ${ln}: invalid link target ${targetName}`);
            links++;
            ops.push({ op: "choice", label, target });
          }
        }
        if (emitted || (rp.lines.length > 1 && !line.trim() && textLen > 0)) {
          ops.push({ op: "text", text: "\n" }); textLen++;
        }
      }
      if (stack.length) throw new Error(`line ${stack[stack.length - 1].ln}: unclosed <<if>>`);
      if (links > 3) throw new Error(`passage ${rp.name} has ${links} links (max 3)`);
      if (textLen > 600) throw new Error(`passage ${rp.name} exceeds 600 characters`);
      if (rp.end && links > 0) throw new Error(`ending passage ${rp.name} must not have links`);
      if (!rp.end && links === 0) throw new Error(`passage ${rp.name} is a dead end`);
      return { name: rp.name, bg: rp.bg, end: rp.end, ops };
    });

    const visited = new Set([start]), q = [start];
    while (q.length) {
      for (const op of passages[q.shift()].ops) {
        if (op.op === "choice" && !visited.has(op.target)) { visited.add(op.target); q.push(op.target); }
      }
    }
    if (visited.size !== passages.length) throw new Error("story has unreachable passages");
    return { title, start, passages };
  }

  function evalPassage(story, idx, flags, applySets) {
    const p = story.passages[idx], choices = [];
    let text = "";
    for (let pc = 0; pc < p.ops.length;) {
      const o = p.ops[pc];
      if (o.op === "text") { text += o.text; pc++; }
      else if (o.op === "choice") { if (choices.length < 3) choices.push({ label: o.label, target: o.target }); pc++; }
      else if (o.op === "set") { if (applySets) flags[o.flag] = o.val; pc++; }
      else if (o.op === "ifnot") { pc = flags[o.flag] ? pc + 1 : o.target; }
      else if (o.op === "jump") { pc = o.target; }
      else pc++;
    }
    return { text: text.trim(), choices, end: p.end };
  }

  const api = { parseTwee, evalPassage };
  if (typeof module !== "undefined" && module.exports) { module.exports = api; return; }
  root.LarkStory = api;

  let story = null, idx = 0, flags = {};
  function render() {
    if (!story) return;
    document.getElementById("story-title").textContent = story.title;
    const res = evalPassage(story, idx, flags, true);
    document.getElementById("passage-text").textContent = res.text;
    const nav = document.getElementById("choices");
    nav.innerHTML = "";
    for (const ch of res.choices) {
      const btn = document.createElement("button");
      btn.type = "button"; btn.className = "button"; btn.textContent = ch.label;
      btn.addEventListener("click", () => { idx = ch.target; render(); });
      nav.appendChild(btn);
    }
    if (res.end) {
      const p = document.createElement("p"); p.className = "end-note"; p.textContent = "— The End —";
      nav.appendChild(p);
    }
  }
  document.getElementById("btn-restart").addEventListener("click", () => {
    if (!story) return;
    idx = story.start; flags = {}; render();
  });
  fetch("story.twee").then((r) => r.text()).then((twee) => {
    story = parseTwee(twee); idx = story.start; flags = {}; render();
  }).catch((e) => { document.getElementById("passage-text").textContent = String(e); });
})(typeof globalThis !== "undefined" ? globalThis : this);
