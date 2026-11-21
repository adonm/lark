import {getLocal, setLocal} from "./dom.js";
import {state} from "./state.js";

const params = new URL(location.href);
const initialToken = params.searchParams.has("token") ? params.searchParams.get("token") : getLocal("lark-token");
if (params.searchParams.has("token")) {
  params.searchParams.delete("token");
  history.replaceState(null, "", params.pathname + params.search + params.hash);
  setLocal("lark-token", initialToken);
}

function request(token, path, {method = "GET", body, signal} = {}) {
  const headers = {"X-Lark-Token": token};
  if (body && !(body instanceof FormData)) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(body);
  }
  return fetch(path, {method, headers, body, signal, cache:"no-store"});
}

let unauthorized;
function onUnauthorized(handler) { unauthorized = handler; }

async function api(current, path, options) {
  const res = await request(current.token, path, options);
  if (res.status === 401 && state.app === current) {
    unauthorized?.();
  }
  if (!res.ok) throw new Error(`Request failed (${res.status})`);
  return res.status === 204 ? null : res.json();
}

export {api, initialToken, onUnauthorized, request};
