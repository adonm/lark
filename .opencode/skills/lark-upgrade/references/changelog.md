# Lark Upgrade Changelog

Newest first; apply bottom-up (oldest entry to the newest) from the installation's starting state. Always back up `lark.db` and `lark.env` before mutating anything.

Entries below the v2 cutover were pruned once v2 installs no longer needed
them; the v1 record stays in git history.

---

## 2026-11-27 — firmware 0.24.0: push-only OTA and a smaller image

### Affects
- Devices: firmware `0.24.0` on vocat and box3.
- Host: no change. `lark ota` already pushes to `POST /ota/upload`.

### Breaking Changes
- The device's `POST /ota` pull route is gone; a recovery flow must use `POST /ota/upload` or USB.
- The firmware no longer links the HTTPS client, `esp_https_ota`, the Mozilla certificate bundle or the TLS stack. Plain-HTTP `esp_http_server` still serves `/status`, `/ota/upload` and `/reboot`.
- IPv6, mDNS queries and softAP/DHCP-server support are disabled; the boards are STA-only IPv4.

### How to Detect
- `GET /status` reports `firmware` below `0.24.0`, or `POST /ota` still returns `200`/`502` instead of `404`.

### Upgrade Steps
1. Push each board: `./scripts/lark ota --no-build <ip>` (add `--board box3` for a BOX-3). Canary a vocat first.
2. No host, config or database step.

### Verification
- `/status` reports `firmware: "0.24.0"`, `connected: true`, `ota_pending: false`, `reset_reason: "sw"`.
- Frames, touch, TTS and `lark doctor` remain clean.

---

## 2026-11-27 — firmware 0.23.0: transport split and mDNS removal

### Affects
- Devices: firmware `0.23.0` on vocat and box3.
- Host: no change. Protocol, wire bytes and `/status` fields are unchanged.

### Breaking Changes
- None. Boards still register their IP in `HELLO`; nothing resolved `<device-id>.local`, and the unused mDNS responder is gone.
- `net_client.c` is now session logic only; socket setup, TCP framing, the wake pipe and RTP uplink live in `net_transport.c`. No external interface changed.

### Upgrade Steps
1. OTA both board types as usual (canary vocat first, then box3).

### Verification
- `/status` reports `firmware: "0.23.0"`, `connected: true`, `ota_pending: false`.
- Frames, touch input and TTS still work; `lark doctor` reports all checks passed.

---

## 2026-11-27 — separate library provisioner and one-time v2 metadata normalization

### Affects
- Host: `lark-edge`, new `lark-library`, and `~/.local/share/lark/lark.db` meta rows.
- Devices: no firmware change.

### Breaking Changes
- `lark-edge library` is gone. `lark library --download|--build` now runs the separate `lark-library` binary that `lark deploy` installs next to the edge.
- The brain no longer reads v1 metadata shapes. Before starting it against an old database, convert every `remote_route:*` row to camelCase (`companionId`, `routeName`, `url`, `localPort`, `profile`, `createdAt`) and every `remote_token:*` row to `{"hash": "<unchanged sha256>", "name": "<display name>"}`. Preserve hashes and URLs verbatim so issued `/remote` links keep working.
- Delta Chat history rows must carry the contact address in `messages.peer`; rows without it stay hidden rather than guessing from a display name.

### Upgrade Steps
1. Back up `lark.db`, then normalize the rows above while the brain is stopped.
2. Run `PRAGMA integrity_check;` and require `ok`.
3. Run `lark deploy` to install both binaries.

### Verification
- `lark library --plan` works and `ls ~/.local/share/lark/lark-library` exists.
- Restored `/remote` links authorize, and history shows both sides of the scoped chat.

---

## 2026-11-26 — firmware 0.22.0: instant reconnect and link reliability

### Affects
- Devices: firmware `0.22.0` on vocat and box3.
- Host: `lark-edge` and `lark-brain` rebuild. No config, database or wire-format change.

### Breaking Changes
- None. The protocol version and every wire byte are unchanged; `/status` only gains fields.

### How to Detect
- `GET /status` lacks `reset_reason`, `wifi_disconnects`, `reconnects` or `last_net_errno`, or reports firmware below `0.22.0`.
- After a board reboot, `lark doctor` takes 30–105 s to show it online and `journalctl --user -u lark-edge` reports `RejectOccupied`.

### Upgrade Steps
1. Deploy the server: `lark deploy`.
2. OTA each board with `lark ota --no-build <ip>`; add `--board box3` for a BOX-3.

### Verification
- `/status` reports `firmware: "0.22.0"`, `reset_reason`, `wifi_disconnects`, `reconnects`, `last_net_errno`, `connected: true` and `ota_pending: false`.
- A board reset reconnects in about 1 s with `terminal.connected ... reconnects=N` in the edge journal and no `RejectOccupied`.
- A sleeping board sends telemetry every 15 s; awake boards keep 5 s.

---

## 2026-11-26 — v2: Go edge + TypeScript brain

### Affects
- Host: systemd units, `~/.config/lark/lark.env`, `~/.local/share/lark/`, and the operator CLI.
- Devices: no firmware change. The v1 terminal protocol and wire bytes are unchanged.

### Breaking Changes
- The single `lark` server binary is gone. `lark-edge.service` (Go) owns terminals, speech models, capture DSP, OTA push, and bulk library builds; `lark-brain.service` (Node 26) owns HTTP, SQLite, Delta Chat, stories, timers, and embedded OpenCode.
- `lark-opencode.service` is removed. OpenCode runs in-process inside the brain through `@opencode/sdk`; there is no loopback `:49375` API, Basic-auth password, or `LARK_OPENCODE_URL`/`LARK_OPENCODE_PASSWORD` setting.
- OpenCode models are code defaults in `brain/src/opencode.ts`; the generated `opencode.jsonc`, `config/opencode/` plugins, and `setup-opencode-config.py` no longer exist. The brain imports the user's active `opencode` provider credential once, from `auth.json` or from the OpenCode SQLite store when the OAuth login lives only there.
- The local library no longer provisions itself on server launch. `lark library --plan` reads the plan; `lark library --download --build` provisions through `lark-edge`.
- Removed CLI commands: `serve`, `mcp`, `sim`, `device`, `stt-bench`. Kept: `status`, `doctor`, `companions`, `terminals`, `tunnels`, `capture`, `library`, `ota`, `deploy`.
- The Go live/QA/bench suites and their stub/fixture harness are removed. The gate now runs the TypeScript brain, including a real Go-edge ↔ TS-brain cross-process test.

### How to Detect
- `systemctl --user status lark.service lark-opencode.service` shows the v1 units, or `lark --help` lists `serve`/`mcp`.
- `~/.local/share/lark/lark` is an ELF binary rather than the CLI wrapper symlink.

### Upgrade Steps
1. Back up `lark.db` and `lark.env`.
2. Stop and disable the v1 units:
   ```sh
   systemctl --user disable --now lark.service lark-opencode.service
   ```
3. From the updated checkout, run:
   ```sh
   ./scripts/setup-host.sh
   ```
   It builds and installs `lark-edge`, runs `npm ci` for the brain, installs the `lark` CLI wrapper, renders and starts `lark-edge.service` and `lark-brain.service`, removes the retired unit files, and strips the old OpenCode password/URL keys from `lark.env`.
4. No database migration and no firmware flash. The brain creates the same schema the v1 server used and opens the existing `lark.db`; stale v1 OpenCode session ids are replaced on the next turn.
5. If you kept a custom `lark.env`, recheck it: `LARK_OPENCODE_*` and `OPENCODE_SERVER_PASSWORD` are ignored and removed; `LARK_DATA_DIR`, `LARK_EDGE_SOCK`, `LARK_DC_RPC_BIN`, and `LARK_REPO_DIR` are set by the units.

### Verification
- `systemctl --user is-active lark-edge.service lark-brain.service` both print `active`; `lark.service` and `lark-opencode.service` are gone.
- `lark doctor` reports all checks passed, including the edge socket and each model.
- `lark status` lists companions and terminals from the existing database; a paired terminal reconnects without re-pairing.
- `lark library --plan` reports the previously installed packs as `installed`.
- `./scripts/check.sh` and `./scripts/check.sh --remote-ui` pass.

---

## 2026-11-23 — Delta Chat Status-Update Wire Log Purge and QA Account Prune

### Affects
- Host / SQLite (`dc.db`) and `~/.local/share/lark/deltachat/` account dirs

### Breaking Changes
- None; one-time housekeeping. Do not remove trash tombstones.

### How to Detect
```sh
DB=~/.local/share/lark/deltachat/216ef3f6-f7b0-4e73-9cdc-c952af9ec643/dc.db
sqlite3 "file:$DB?mode=ro" "SELECT name, SUM(pgsize)/1048576 AS mb FROM dbstat GROUP BY name ORDER BY mb DESC LIMIT 5;"
sqlite3 "file:$DB?mode=ro" "SELECT COUNT(*), SUM(LENGTH(param))/1048576, SUM(state=20) FROM msgs WHERE param LIKE '%S=30%';"
```
A healthy account has single-digit `S=30` rows. Thousands means a panel echo or a stalled `smtp` queue.

### Upgrade Steps
1. Stop `lark-brain.service` and `lark-edge.service`. Reflink-backup the account dir (`cp -a --reflink=always`).
2. Delete the stale wire log and its queue, then vacuum:
   ```sql
   DELETE FROM smtp WHERE msg_id IN (SELECT id FROM msgs WHERE param LIKE '%S=30%');
   DELETE FROM smtp WHERE msg_id NOT IN (SELECT id FROM msgs);
   DELETE FROM msgs WHERE param LIKE '%S=30%';
   VACUUM;
   PRAGMA integrity_check;
   ```
   The `delete_messages` RPC only trashes rows (zeroed tombstones), so direct SQL is required to reclaim space.
3. Prune unreferenced accounts with a one-off `RemoveAccount` client (`DC_ACCOUNTS_PATH` must point at `~/.local/share/lark/deltachat`, service stopped). Keep the `dc_account_id`s still present in `companions`; remove QA companions first (`lark companions rm <id>`), then restart both services.

### Verification
- `PRAGMA integrity_check` returns `ok`; the `S=30` count is single-digit and `smtp` drains to 0.
- `lark status` lists the household companions.

