const $ = id => document.getElementById(id);
const screen = $("stage"), nodes = $("nodes"), svgNS = "http://www.w3.org/2000/svg";
const getLocal = key => { try { return localStorage.getItem(key) || ""; } catch { return ""; } };
const setLocal = (key, val) => { try { localStorage.setItem(key, val); } catch {} };

const wide = matchMedia("(min-width: 921px)");
function colors(el, companion) {
  if (/^#[a-f\d]{6}$/i.test(companion?.accent_color || "")) el.style.setProperty("--companion-accent", companion.accent_color);
  else el.style.removeProperty("--companion-accent");
}
const isLive = c => !!c?.online;
const companionName = c => c?.name || c?.character || "New Companion";
const node = (tag, cls, text) => {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text != null) element.textContent = text;
  return element;
};
const say = (id, text) => {
  const element = $(id);
  if (element.textContent !== text) element.textContent = text;
};

export {$, colors, companionName, getLocal, isLive, node, nodes, say, screen, setLocal, svgNS, wide};
