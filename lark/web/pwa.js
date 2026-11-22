export function installAssets(view, app) {
  if (!view?.id || !app) return null;
  const query = new URLSearchParams(app.scope ? {token:app.token} : {companion_id:view.id});
  const base = "/app/pwa/";
  return {manifest:`${base}manifest.webmanifest?${query}`, icon:`${base}icon-192.png?${query}`};
}

export function installer() {
  const manifest = document.getElementById("companion-manifest"), icon = document.getElementById("companion-icon");
  const install = document.getElementById("install-companion"), hint = document.getElementById("companion-install-hint");
  const themes = [...document.querySelectorAll("meta[name=theme-color]")].map(meta => [meta, meta.content]);
  const standalone = matchMedia("(display-mode: standalone)");
  const ios = /iPad|iPhone|iPod/i.test(navigator.userAgent) || navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1;
  let prompt, registered = false, installed = false;

  function render() {
    const show = manifest.hasAttribute("href") && !installed && !standalone.matches && !navigator.standalone;
    install.hidden = !show || !prompt;
    hint.hidden = !show || !ios || !!prompt;
  }

  if (window.isSecureContext) {
    window.addEventListener("beforeinstallprompt", event => {
      if (!manifest.hasAttribute("href")) return;
      event.preventDefault();
      prompt = event;
      render();
    });
    window.addEventListener("appinstalled", () => { installed = true; prompt = null; render(); });
    standalone.addEventListener("change", render);
    install.onclick = async () => {
      const current = prompt;
      prompt = null;
      render();
      try { await current?.prompt(); } catch { /* The browser can still offer installation in its menu. */ }
    };
  }

  return (view, app) => {
    const assets = window.isSecureContext && installAssets(view, app);
    const mark = assets && /^#[a-f\d]{6}$/i.test(view.companion?.mark_color) && view.companion.mark_color;
    for (const [meta, fallback] of themes) meta.content = mark || fallback;
    if (assets?.manifest === (manifest.getAttribute("href") || undefined)) return;
    prompt = null;
    installed = false;
    if (assets) {
      manifest.setAttribute("href", assets.manifest);
      icon.setAttribute("href", assets.icon);
      if (!registered && navigator.serviceWorker) {
        registered = true;
        navigator.serviceWorker.register("/app/sw.js", {scope:"/app"}).catch(() => {});
      }
    } else {
      manifest.removeAttribute("href");
      icon.removeAttribute("href");
    }
    render();
  };
}
