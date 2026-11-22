app = (() => {
  "use strict";
  const content = storeGet("content");
  const d = {...templateData, ...content};
  const title = content?.title || templateTitle;
  const themeName = d.theme || "warm";
  const themes = {
    warm:  {bg: "#1E1A2B", tile: "#2A2338", border: "#705F7C", accent: "#FFC1A8", ink: "#FAF0F6", muted: "#C5B6CC"},
    night: {bg: "#141624", tile: "#1F2338", border: "#4F5D88", accent: "#9CBFE8", ink: "#EEF2FA", muted: "#9AA7C4"},
    mint:  {bg: "#152420", tile: "#1E342D", border: "#4E8272", accent: "#9AE5C3", ink: "#F0FAF5", muted: "#9CC2B4"},
    sky:   {bg: "#182232", tile: "#223047", border: "#557299", accent: "#9CBFE8", ink: "#F3F7FD", muted: "#A5B8D4"},
  };
  const pal = themes[themeName] || themes.warm;
  const defaults = ["#CB6B8F", "#D5A051", "#528A77", "#687DAF"];
  const assert = (ok, msg) => { if (!ok) throw new TypeError(msg); };
  const count = (a, lo, hi) => Array.isArray(a) && a.length >= lo && a.length <= hi;
  const integer = (n, lo, hi) => Number.isInteger(n) && n >= lo && n <= hi;
  const label = (s, n) => typeof s === "string" && s.trim() && s.length <= n;
  const color = s => /^#[0-9a-f]{6}$/i.test(s);
  const colors = () => {
    const c = d.colors || defaults;
    assert(count(c, 2, 4) && c.every(color), "colors requires 2–4 #RRGGBB colors");
    return c;
  };
  const tap = () => chime("tap");
  const win = () => chime("celebrate");

  const header = (g, sub) => {
    g.clear(pal.bg);
    const sx = g.safe.x, sy = g.safe.y, sw = g.safe.w;
    g.text(sx + Math.floor(sw / 2), sy + 2, title.slice(0, g.round ? 14 : 24), {color: pal.accent, align: "center", bold: true});
    if (sub) {
      g.text(sx + Math.floor(sw / 2), sy + 12, String(sub), {color: pal.muted, align: "center"});
    }
  };

  const exitBtn = (g, lbl = "Back") => {
    const bh = g.round ? 12 : 16;
    const bw = g.round ? 34 : 40;
    const bx = g.safe.x;
    const by = g.safe.y + g.safe.h - bh;
    g.button("exit", bx, by, bw, bh, lbl, pal.tile, pal.border, pal.ink);
    return {bx, by, bw, bh};
  };

  const tileArt = (g, name, x, y) => {
    if (name === "circle") g.rect(x + 1, y + 1, 8, 8, "#9CBFE8", pal.border);
    else if (name === "square") g.rect(x + 1, y + 1, 8, 8, "#FFC1A8", pal.border);
    else if (name === "triangle") g.rect(x + 2, y + 2, 6, 6, "#9AE5C3", pal.border);
    else if (name === "diamond") g.rect(x + 2, y + 2, 6, 6, "#BFA8E5", pal.border);
    else g.sprite(x, y, name, null, 1);
  };

  function questions(qs, picture) {
    const fresh = () => ({q: 0, score: 0, picked: -1});
    return {
      init: fresh,
      render(s, _p, g) {
        header(g, s.q >= qs.length ? "All done" : `${s.q + 1}/${qs.length}`);
        const sx = g.safe.x, sy = g.safe.y, sw = g.safe.w, sh = g.safe.h;
        const nav = exitBtn(g);
        if (s.q >= qs.length) {
          g.rect(sx, sy + 22, sw, 26, pal.tile, pal.border);
          g.text(sx + Math.floor(sw / 2), sy + 30, `${s.score} of ${qs.length}`, {color: pal.ink, align: "center", bold: true});
          g.button("again", sx + nav.bw + 4, nav.by, sw - nav.bw - 4, nav.bh, "Play again", pal.tile, pal.accent, pal.ink);
          return;
        }
        const q = qs[s.q];
        const answered = s.picked >= 0;
        const promptY = sy + 20;
        if (picture && !answered) {
          for (const [n, py, name] of [[q.left, promptY, "apple"], [q.right, promptY + 11, "fish"]]) {
            for (let i = 0; i < n; i++) {
              const px = sx + Math.floor(sw / 2) + Math.round((i - (n - 1) / 2) * 11) - 5;
              g.sprite(px, py, name, null, 1);
            }
          }
          g.text(sx + Math.floor(sw / 2), promptY + 23, "How many?", {color: pal.ink, align: "center"});
        } else {
          const msg = answered ? (q.answer === s.picked ? "Correct!" : `Ans: ${q.choices[q.answer]}`) : q.prompt;
          g.text(sx + Math.floor(sw / 2), promptY + 2, msg.slice(0, g.round ? 14 : 24), {color: pal.ink, align: "center"});
        }

        if (answered) {
          g.button("next", sx + nav.bw + 4, nav.by, sw - nav.bw - 4, nav.bh, s.q + 1 === qs.length ? "Results" : "Next", pal.tile, pal.accent, pal.ink);
        } else {
          const topY = picture ? promptY + 34 : promptY + 14;
          const availH = nav.by - 3 - topY;
          const n = q.choices.length;
          const rowH = Math.min(16, Math.floor((availH - (n - 1) * 2) / n));
          for (let i = 0; i < n; i++) {
            g.button("answer-" + i, sx, topY + i * (rowH + 2), sw, rowH, q.choices[i], pal.tile, pal.border, pal.ink);
          }
        }
      },
      onTap(s, _, id) {
        if (id === "again") { tap(); return fresh(); }
        if (id === "next") { tap(); return {...s, q: s.q + 1, picked: -1}; }
        const q = qs[s.q];
        const i = Number(id.slice(7));
        if (q && s.picked < 0 && id.startsWith("answer-") && integer(i, 0, q.choices.length - 1)) {
          const correct = i === q.answer;
          if (correct) win(); else tap();
          return {...s, picked: i, score: s.score + Number(correct)};
        }
      },
    };
  }

  switch (templateName) {
  case "quiz": {
    const qs = d.questions || [{prompt: "Which one can fly?", choices: ["Fish", "Bird", "Dog"], answer: 1}];
    assert(count(qs, 1, 32) && qs.every(q => label(q.prompt, 200) && count(q.choices, 2, 4) &&
      q.choices.every(c => label(c, 64)) && integer(q.answer, 0, q.choices.length - 1)), "quiz needs 1–32 questions with 2–4 choices and a zero-based answer");
    return questions(qs, false);
  }
  case "picture-math": {
    const ps = d.problems || [{left: 2, right: 3}, {left: 4, right: 1}, {left: 3, right: 4}];
    assert(count(ps, 1, 32) && ps.every(p => integer(p.left, 0, 8) && integer(p.right, 0, 8)), "picture-math needs 1–32 problems with 0–8 pictures per row");
    return questions(ps.map((p, i) => {
      const n = p.left + p.right, answer = i % 3, choices = [n + 1, n + 2];
      choices.splice(answer, 0, n);
      return {...p, prompt: "How many?", answer, choices};
    }), true);
  }
  case "spot-the-difference": {
    const tiles = d.tiles || [
      {left: "owl", right: "owl"}, {left: "gift", right: "gift"}, {left: "sun", right: "moon"},
      {left: "heart", right: "heart"}, {left: "rocket", right: "rocket"}, {left: "fox", right: "fox"},
    ];
    assert(count(tiles, 2, 6) && tiles.some(t => t.left !== t.right), "spot-the-difference needs 2–6 tile pairs with at least one difference");
    const fresh = () => ({found: {}, done: false});
    return {
      init: fresh,
      render(s, _p, g) {
        const diffCount = tiles.filter(t => t.left !== t.right).length;
        const foundCount = Object.keys(s.found).length;
        header(g, s.done ? "All found!" : `${foundCount}/${diffCount} found`);
        const nav = exitBtn(g);
        const sx = g.safe.x, sy = g.safe.y + 20, sw = g.safe.w;
        const gap = 3, sideW = Math.floor((sw - gap) / 2);
        const rows = Math.ceil(tiles.length / 2);
        const cellW = Math.floor(sideW / 2) - 1;
        const cellH = Math.min(20, Math.floor((nav.by - 2 - sy) / rows) - 2);
        tiles.forEach((t, i) => {
          const col = i % 2, row = Math.floor(i / 2);
          const xL = sx + col * cellW;
          const xR = sx + sideW + gap + col * cellW;
          const y = sy + row * (cellH + 2);
          const mark = s.found[i] ? pal.accent : pal.border;
          g.button("t-" + i, xL, y, cellW - 1, cellH, "", pal.tile, mark);
          g.button("r-" + i, xR, y, cellW - 1, cellH, "", pal.tile, mark);
          tileArt(g, t.left, xL + Math.floor((cellW - 11) / 2), y + Math.floor((cellH - 10) / 2));
          tileArt(g, t.right, xR + Math.floor((cellW - 11) / 2), y + Math.floor((cellH - 10) / 2));
        });
        if (s.done) {
          g.button("again", sx + nav.bw + 4, nav.by, sw - nav.bw - 4, nav.bh, "Again", pal.tile, pal.accent, pal.ink);
        }
      },
      onTap(s, _, id) {
        if (id === "again") { tap(); return fresh(); }
        if (!id.startsWith("t-") && !id.startsWith("r-")) return;
        const i = Number(id.slice(2));
        const t = tiles[i];
        if (!t || t.left === t.right || s.found[i]) { tap(); return; }
        const found = {...s.found, [i]: true};
        const done = tiles.every((x, j) => x.left === x.right || found[j]);
        if (done) win(); else tap();
        return {found, done};
      },
    };
  }
  case "drawing": {
    const cs = colors();
    const cell = 7, cols = 8, rows = 6;
    const fresh = () => ({color: cs[0], grid: Array(cols * rows).fill("")});
    return {
      init: fresh,
      render(s, _p, g) {
        header(g, "");
        const nav = exitBtn(g);
        const gridW = cols * cell, gridH = rows * cell;
        const gx = g.safe.x + Math.floor((g.safe.w - gridW) / 2);
        const gy = g.safe.y + 13;
        for (let r = 0; r < rows; r++) {
          for (let c = 0; c < cols; c++) {
            const idx = r * cols + c;
            const fill = s.grid[idx] || pal.tile;
            g.button("c-" + idx, gx + c * cell, gy + r * cell, cell - 1, cell - 1, "", fill, pal.border);
          }
        }
        const palY = gy + gridH + 3;
        const swW = 14;
        cs.forEach((c, i) => {
          const bx = g.safe.x + i * (swW + 2);
          g.button("p-" + i, bx, palY, swW, 10, "", c, s.color === c ? pal.ink : pal.border);
        });
        g.button("clear", g.safe.x + nav.bw + 4, nav.by, g.safe.w - nav.bw - 4, nav.bh, "Clear", pal.tile, pal.border, pal.ink);
      },
      onTap(s, _, id) {
        if (id === "clear") { tap(); return {...s, grid: Array(cols * rows).fill("")}; }
        if (id.startsWith("p-")) { tap(); return {...s, color: cs[Number(id.slice(2))] || s.color}; }
        if (id.startsWith("c-")) {
          const idx = Number(id.slice(2));
          if (integer(idx, 0, cols * rows - 1)) {
            const grid = s.grid.slice();
            grid[idx] = s.color;
            return {...s, grid};
          }
        }
      },
    };
  }
  case "pet": {
    const petName = d.name || "Pebble";
    assert(label(petName, 24), "pet name must be 1–24 characters");
    const petColor = d.pet_color || "#9AE5C3";
    assert(color(petColor), "pet_color must be #RRGGBB");
    const fresh = () => ({hunger: 3, mood: 3, msg: "Happy"});
    return {
      init: fresh,
      render(s, _p, g) {
        header(g, petName);
        const nav = exitBtn(g);
        const sx = g.safe.x, sy = g.safe.y + 20, sw = g.safe.w;
        g.rect(sx, sy, sw, 34, pal.tile, pal.border);
        const cx = sx + Math.floor(sw / 2) - 5;
        g.sprite(cx, sy + 5, "fox", {p: petColor}, 1);
        g.text(sx + Math.floor(sw / 2), sy + 20, `${s.msg} (${s.hunger}/5)`, {color: pal.ink, align: "center"});
        const bw = Math.floor((sw - 4) / 2);
        const by = nav.by - 16;
        g.button("feed", sx, by, bw, 14, "Feed", pal.tile, pal.accent, pal.ink);
        g.button("play", sx + bw + 4, by, bw, 14, "Play", pal.tile, pal.border, pal.ink);
      },
      onTap(s, _, id) {
        if (id === "feed") { tap(); return {hunger: Math.min(5, s.hunger + 1), mood: s.mood, msg: "Yum!"}; }
        if (id === "play") { win(); return {hunger: Math.max(0, s.hunger - 1), mood: Math.min(5, s.mood + 1), msg: "Yay!"}; }
      },
    };
  }
  case "timer": {
    const secs = d.seconds ?? 60;
    assert(integer(secs, 5, 3600), "seconds must be 5–3600");
    const fresh = () => ({left: secs, running: false, done: false});
    const fmt = n => `${Math.floor(n / 60)}:${String(n % 60).padStart(2, "0")}`;
    return {
      init: fresh,
      render(s, _p, g) {
        header(g, s.done ? "Time's up!" : (s.running ? "Running" : "Ready"));
        const nav = exitBtn(g);
        const sx = g.safe.x, sy = g.safe.y + 22, sw = g.safe.w;
        g.rect(sx, sy, sw, 28, pal.tile, pal.border);
        g.text(sx + Math.floor(sw / 2), sy + 8, fmt(s.left), {color: pal.accent, size: "title", align: "center"});
        const bw = Math.floor((sw - 4) / 2);
        const by = nav.by - 16;
        g.button("toggle", sx, by, bw, 14, s.running ? "Pause" : "Start", pal.tile, pal.accent, pal.ink);
        g.button("reset", sx + bw + 4, by, bw, 14, "Reset", pal.tile, pal.border, pal.ink);
      },
      onTap(s, _, id) {
        if (id === "reset") { tap(); return fresh(); }
        if (id === "toggle") { tap(); return s.done ? {...fresh(), running: true} : {...s, running: !s.running}; }
      },
      onTick(s) {
        if (!s.running || s.left <= 0) return;
        const left = s.left - 1;
        if (left === 0) { win(); return {left: 0, running: false, done: true}; }
        return {...s, left};
      },
    };
  }
  case "memory-match": {
    const pairs = d.cards || ["apple", "star", "moon", "heart"];
    assert(count(pairs, 2, 4) && pairs.every(c => label(c, 16)), "memory-match needs 2–4 card labels");
    const deck = pairs.flatMap((v, i) => [v, v]).map((v, i) => ({v, k: i}));
    const fresh = () => ({open: [], matched: {}, moves: 0});
    return {
      init: fresh,
      render(s, _p, g) {
        const done = Object.keys(s.matched).length === deck.length;
        header(g, done ? `Done (${s.moves})` : `Moves: ${s.moves}`);
        const nav = exitBtn(g);
        const cols = 4, rows = Math.ceil(deck.length / cols);
        const sx = g.safe.x, sy = g.safe.y + 20, sw = g.safe.w;
        const cw = Math.floor((sw - (cols - 1) * 2) / cols);
        const ch = Math.min(22, Math.floor((nav.by - 4 - sy) / rows) - 2);
        deck.forEach((card, i) => {
          const r = Math.floor(i / cols), c = i % cols;
          const x = sx + c * (cw + 2), y = sy + r * (ch + 2);
          const up = s.matched[i] || s.open.includes(i);
          g.button("m-" + i, x, y, cw, ch, up ? "" : "?", up ? pal.bg : pal.tile, s.matched[i] ? pal.accent : pal.border);
          if (up) tileArt(g, card.v, x + Math.floor((cw - 10) / 2), y + Math.floor((ch - 10) / 2));
        });
        if (done) {
          g.button("again", sx + nav.bw + 4, nav.by, sw - nav.bw - 4, nav.bh, "Again", pal.tile, pal.accent, pal.ink);
        }
      },
      onTap(s, _, id) {
        if (id === "again") { tap(); return fresh(); }
        if (!id.startsWith("m-")) return;
        const i = Number(id.slice(2));
        if (!integer(i, 0, deck.length - 1) || s.matched[i] || s.open.includes(i)) return;
        tap();
        const open = s.open.length === 2 ? [i] : [...s.open, i];
        const moves = s.moves + 1;
        if (open.length === 2 && deck[open[0]].v === deck[open[1]].v) {
          const matched = {...s.matched, [open[0]]: true, [open[1]]: true};
          if (Object.keys(matched).length === deck.length) win();
          return {open: [], matched, moves};
        }
        return {...s, open, moves};
      },
    };
  }
  case "fireworks": {
    const cs = colors();
    const fresh = () => ({bursts: [{x: 60, y: 45, c: cs[0], t: 0}]});
    return {
      init: fresh,
      render(s, _p, g) {
        header(g, "Tap sky!");
        const nav = exitBtn(g);
        g.hit("sky", g.safe.x, g.safe.y + 18, g.safe.w, nav.by - (g.safe.y + 18));
        for (const b of s.bursts) {
          const r = (b.t % 5) * 4 + 3;
          g.rect(b.x - r, b.y, 2, 2, b.c);
          g.rect(b.x + r, b.y, 2, 2, b.c);
          g.rect(b.x, b.y - r, 2, 2, b.c);
          g.rect(b.x, b.y + r, 2, 2, b.c);
          g.rect(b.x - Math.floor(r * 0.7), b.y - Math.floor(r * 0.7), 2, 2, b.c);
          g.rect(b.x + Math.floor(r * 0.7), b.y + Math.floor(r * 0.7), 2, 2, b.c);
        }
      },
      onTap(s, _, id, x, y) {
        if (id !== "sky") return;
        win();
        const lx = Math.round(x / (gScaleGuess()));
        const ly = Math.round(y / (gScaleGuess()));
        const c = cs[s.bursts.length % cs.length];
        return {bursts: [...s.bursts.slice(-4), {x: lx || 60, y: ly || 50, c, t: 0}]};
      },
      onTick(s) {
        return {bursts: s.bursts.map(b => ({...b, t: (b.t + 1) % 6}))};
      },
    };
  }
  default:
    throw new Error("unknown template " + templateName);
  }
  function gScaleGuess() { return 2; }
})();
