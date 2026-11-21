import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {runInNewContext} from "node:vm";
import {test} from "node:test";
import {installAssets} from "./pwa.js";

test("LAN install assets use a companion ID, scoped assets use only the existing share grant", () => {
  const app = {token:"fixture", scope:null}, view = {virtual:true, id:"phone-1234"};
  assert.equal(installAssets({...view, virtual:false}, app), null);
  assert.deepEqual(installAssets(view, app), {
    manifest:"/app/pwa/manifest.webmanifest?device_id=phone-1234",
    icon:"/app/pwa/icon-192.png?device_id=phone-1234",
  });
  assert.deepEqual(installAssets(view, {...app, scope:{device_id:"virt-12345678"}}), {
    manifest:"/app/pwa/manifest.webmanifest?token=fixture",
    icon:"/app/pwa/icon-192.png?token=fixture",
  });
});

test("worker caches only the versioned shell; runtime navigation never writes private URLs", async () => {
  const listen = {}, calls = [], origin = "https://fixture.invalid";
  const offlineShell = new Response("offline shell"), online = new Response("online"), cache = "lark-shell-scoped-new";
  let offline = false;
  const context = {
    URL, Request, Response,
    self:{location:{origin, href:`${origin}/app/sw.js`}, addEventListener: (name, handler) => listen[name] = handler,
      clients:{claim:async () => {}}},
    caches:{
      async open(name) {
        assert.equal(name, cache);
        return {
          addAll: async files => calls.push(["precache", ...Array.from(files, file => new URL(file.url).pathname)]),
          match: async path => { calls.push(["match", path]); return offlineShell; },
        };
      },
      keys:async () => ["lark-shell-scoped-old", cache, "other-app"],
      delete:async key => calls.push(["delete", key]),
    },
    fetch:async request => {
      calls.push(["fetch", new URL(request.url).pathname]);
      if (offline) throw new Error("Offline");
      return online;
    },
  };
  runInNewContext(`const CACHE = "${cache}", SHELL = ["/app", "/app/client.js", "/app/style.css", "/app/lark.svg"];\n` +
    readFileSync(new URL("./sw.js", import.meta.url), "utf8"), context);
  let done;
  listen.install({waitUntil: promise => done = promise});
  await done;
  listen.activate({waitUntil: promise => done = promise});
  await done;
  assert.deepEqual(calls, [["precache", "/app", "/app/client.js", "/app/style.css", "/app/lark.svg"],
    ["delete", "lark-shell-scoped-old"]]);
  calls.length = 0;

  function request(path, mode = "cors", method = "GET") {
    let response;
    listen.fetch({request:{url:new URL(path, origin).href, mode, method}, respondWith: promise => response = promise});
    return response;
  }
  for (const path of ["/api/scope", "/api/devices/virt-12345678/messages", "/api/audio/fixture",
    "/api/photos/fixture", "/ws", "/app/client.js?q=other", "/app/parent", "/app/photo-library.js",
    "/app/pwa/manifest.webmanifest", "/app/pwa/icon-192.png", "https://other.invalid/app/client.js",
    "https://unpkg.com/@knadh/oat@1.1.0/oat.min.css", "https://unpkg.com/@knadh/oat@1.1.0/oat.min.js"]) {
    assert.equal(request(path), undefined);
  }
  assert.equal(request("/app", "navigate", "POST"), undefined);
  assert.deepEqual(calls, []);
  assert.equal(await request("/app?token=fixture", "navigate"), online);
  assert.equal(await request("/app/client.js"), online);
  offline = true;
  assert.equal(await request("/app?token=fixture", "navigate"), offlineShell);
  assert.deepEqual(calls, [["fetch", "/app"], ["fetch", "/app/client.js"], ["fetch", "/app"], ["match", "/app"]]);
});
