// The bridge versions this same-origin shell by its contents. Oat can use the
// browser's HTTP cache; native controls still work if the CDN is unreachable.
self.addEventListener("install", event => {
  const files = SHELL.map(path => new Request(new URL(path, self.location.href), {redirect:"error", cache:"reload"}));
  event.waitUntil(caches.open(CACHE).then(cache => cache.addAll(files)));
});

self.addEventListener("activate", event => {
  event.waitUntil(caches.keys().then(keys => Promise.all(keys
    .filter(key => key !== CACHE && key.startsWith("lark-shell-"))
    .map(key => caches.delete(key)))).then(() => self.clients.claim()));
});

self.addEventListener("fetch", event => {
  const request = event.request, url = new URL(request.url);
  if (request.method !== "GET" || url.origin !== self.location.origin) return;
  const navigation = request.mode === "navigate" && url.pathname === "/app";
  if (!navigation && (url.search || url.pathname === "/app" || !SHELL.includes(url.pathname))) return;
  // Only install-time, token-free shell responses enter Cache Storage.
  event.respondWith(fetch(request).catch(async () =>
    (await caches.open(CACHE)).match(url.pathname).then(response => response || Response.error())));
});
