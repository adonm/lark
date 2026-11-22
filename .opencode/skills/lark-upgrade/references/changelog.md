# Lark Upgrade Changelog

Apply entries chronologically from the installation's starting state to latest. Always back up `lark.db` and `lark.env` (or `bridge.env`) before mutating anything.

---

## 2026-11-22 — Companion / Terminal Identity Split (`0.11.0`)

### Affects
- SQLite / Lark API & MCP / Web / Firmware

### Breaking Changes
- **Companions vs Terminals**: `companions` is now keyed by lowercase slug `id` (with `voice` collapsed in; `saved_companions` and `voices` are removed). `devices` and `virtual_clients` are replaced by `terminals` (`vocat-<mac4>`, `box3-<mac4>`, `web-<8hex>`) with nullable `companion_id`. `device_prefs` is renamed to `terminal_prefs`.
- **Companion-owned tables**: `sessions`, `messages`, `timers`, `build_jobs`, `parent_settings`, `parent_usage`, and `tunnels` are re-keyed from `device_id` to `companion_id`. Memory scopes for companions use `companion:<id>`.
- **Routes & MCP**: `/api/devices*`, `/api/virtual-clients*`, and `/api/companions/reassign` are removed in favor of `/api/companions*` and `/api/terminals*` (`attach`/`detach`). MCP `state`/`patch`/`app`/`emit` address `companion_id` (`me.companion` + `me.terminals[]`).
- **Firmware (`0.11.0`)**: `BOARD_ID_PREFIX` for `vocat` is changed from `lark-` to `vocat-`.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db ".tables"` shows `devices`, `virtual_clients`, `saved_companions`, or `device_prefs`.

### Upgrade Steps
1. Back up `~/.local/share/lark/lark.db` and stop `lark.service`:
   ```sh
   systemctl --user stop lark.service
   cp -a ~/.local/share/lark/lark.db ~/.local/share/lark/backups/pre-redesign-b.db
   ```
2. Run the following SQL in one transaction (adjust `id_map` for the installation's device-to-companion mappings; `virt-8458606f` migrates to `web-8458606f` attached to `george` so volume prefs and tunnel continuity are preserved):
   ```sql
   PRAGMA foreign_keys = OFF;
   BEGIN IMMEDIATE;

   CREATE TEMP TABLE id_map (old_id TEXT PRIMARY KEY, companion_id TEXT NOT NULL, terminal_id TEXT NOT NULL);
   INSERT INTO id_map (old_id, companion_id, terminal_id) VALUES
     ('lark-33d8', 'cake', 'vocat-33d8'),
     ('lark-2cac', 'mint', 'vocat-2cac'),
     ('box3-b324', 'greg', 'box3-b324'),
     ('virt-8458606f', 'george', 'web-8458606f');

   CREATE TABLE companions_new (
     id TEXT PRIMARY KEY,
     name TEXT NOT NULL,
     configured INTEGER NOT NULL DEFAULT 0,
     face_bg TEXT NOT NULL,
     mark_color TEXT NOT NULL,
     accent_color TEXT NOT NULL,
     personality TEXT NOT NULL DEFAULT '',
     voice TEXT NOT NULL DEFAULT '',
     updated_at INTEGER NOT NULL,
     face_svg TEXT NOT NULL DEFAULT ''
   );

   INSERT INTO companions_new (id, name, configured, face_bg, mark_color, accent_color, personality, voice, updated_at, face_svg)
   SELECT
     m.companion_id,
     c.name,
     c.configured,
     c.face_bg,
     c.mark_color,
     c.accent_color,
     c.personality,
     COALESCE((SELECT v.voice FROM voices v WHERE v.device_id = c.device_id), ''),
     c.updated_at,
     c.face_svg
   FROM companions c
   JOIN id_map m ON m.old_id = c.device_id;

   INSERT OR IGNORE INTO companions_new (id, name, configured, face_bg, mark_color, accent_color, personality, voice, updated_at, face_svg)
   SELECT slug, name, 1, face_bg, mark_color, accent_color, personality, voice, updated_at, face_svg
   FROM saved_companions;

   DROP TABLE companions;
   DROP TABLE saved_companions;
   DROP TABLE voices;
   ALTER TABLE companions_new RENAME TO companions;

   CREATE TABLE terminals (
     id TEXT PRIMARY KEY,
     kind TEXT NOT NULL,
     companion_id TEXT REFERENCES companions(id) ON DELETE SET NULL,
     ip TEXT NOT NULL DEFAULT '',
     port INTEGER NOT NULL DEFAULT 0,
     last_seen INTEGER NOT NULL DEFAULT 0,
     created_at TEXT NOT NULL DEFAULT ''
   );

   INSERT INTO terminals (id, kind, companion_id, ip, port, last_seen, created_at)
   SELECT m.terminal_id, 'hardware', m.companion_id, d.ip, d.port, d.last_seen, ''
   FROM devices d
   JOIN id_map m ON m.old_id = d.device_id;

   INSERT INTO terminals (id, kind, companion_id, ip, port, last_seen, created_at)
   SELECT m.terminal_id, 'web', m.companion_id, '', 0, 0, v.created_at
   FROM virtual_clients v
   JOIN id_map m ON m.old_id = v.id;

   DROP TABLE devices;
   DROP TABLE virtual_clients;

   CREATE TABLE terminal_prefs (
     terminal_id TEXT PRIMARY KEY,
     auto_volume INTEGER NOT NULL DEFAULT 1,
     manual_volume INTEGER NOT NULL DEFAULT 55
   );
   INSERT INTO terminal_prefs (terminal_id, auto_volume, manual_volume)
   SELECT COALESCE((SELECT m.terminal_id FROM id_map m WHERE m.old_id = dp.device_id), dp.device_id),
          dp.auto_volume, dp.manual_volume
   FROM device_prefs dp;
   DROP TABLE device_prefs;

   DROP TABLE sessions;
   CREATE TABLE sessions (
     companion_id TEXT PRIMARY KEY,
     session_id TEXT NOT NULL,
     updated_at INTEGER NOT NULL,
     contract_hash TEXT NOT NULL DEFAULT '',
     context_after_ms INTEGER NOT NULL DEFAULT 0
   );

   CREATE TABLE messages_new (
     id TEXT PRIMARY KEY,
     companion_id TEXT NOT NULL,
     role TEXT NOT NULL,
     sender TEXT NOT NULL,
     text TEXT NOT NULL,
     source TEXT NOT NULL,
     card_title TEXT NOT NULL DEFAULT '',
     ts_ms INTEGER NOT NULL
   );
   INSERT INTO messages_new (id, companion_id, role, sender, text, source, card_title, ts_ms)
   SELECT msg.id,
          COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = msg.device_id), msg.device_id),
          msg.role, msg.sender, msg.text, msg.source, msg.card_title, msg.ts_ms
   FROM messages msg;
   DROP TABLE messages;
   ALTER TABLE messages_new RENAME TO messages;
   CREATE INDEX messages_companion_ts_id ON messages (companion_id, ts_ms, id);

   CREATE TABLE timers_new (
     id TEXT PRIMARY KEY,
     companion_id TEXT NOT NULL,
     label TEXT NOT NULL,
     say TEXT NOT NULL,
     chime TEXT NOT NULL,
     wake_agent INTEGER NOT NULL DEFAULT 0,
     created_at TEXT NOT NULL,
     fire_at TEXT NOT NULL,
     notes_json TEXT NOT NULL DEFAULT '[]',
     app_name TEXT NOT NULL DEFAULT ''
   );
   INSERT INTO timers_new (id, companion_id, label, say, chime, wake_agent, created_at, fire_at, notes_json, app_name)
   SELECT t.id,
          COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = t.device_id), t.device_id),
          t.label, t.say, t.chime, t.wake_agent, t.created_at, t.fire_at, t.notes_json, t.app_name
   FROM timers t;
   DROP TABLE timers;
   ALTER TABLE timers_new RENAME TO timers;

   CREATE TABLE build_jobs_new (
     id TEXT PRIMARY KEY,
     companion_id TEXT NOT NULL,
     state TEXT NOT NULL,
     spec TEXT NOT NULL,
     created_at INTEGER NOT NULL,
     result TEXT NOT NULL DEFAULT '',
     announced INTEGER NOT NULL DEFAULT 0,
     cleanup INTEGER NOT NULL DEFAULT 0
   );
   INSERT INTO build_jobs_new (id, companion_id, state, spec, created_at, result, announced, cleanup)
   SELECT b.id,
          COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = b.device_id), b.device_id),
          b.state, b.spec, b.created_at, b.result, b.announced, b.cleanup
   FROM build_jobs b;
   DROP TABLE build_jobs;
   ALTER TABLE build_jobs_new RENAME TO build_jobs;
   CREATE INDEX build_jobs_state ON build_jobs (state, companion_id);

   CREATE TABLE parent_settings_new (
     companion_id TEXT PRIMARY KEY,
     timezone TEXT NOT NULL,
     quiet_start TEXT NOT NULL,
     quiet_end TEXT NOT NULL,
     daily_minutes INTEGER NOT NULL,
     pause_until_ms INTEGER NOT NULL,
     override_until_ms INTEGER NOT NULL
   );
   INSERT INTO parent_settings_new (companion_id, timezone, quiet_start, quiet_end, daily_minutes, pause_until_ms, override_until_ms)
   SELECT COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = ps.device_id), ps.device_id),
          ps.timezone, ps.quiet_start, ps.quiet_end, ps.daily_minutes, ps.pause_until_ms, ps.override_until_ms
   FROM parent_settings ps;
   DROP TABLE parent_settings;
   ALTER TABLE parent_settings_new RENAME TO parent_settings;

   CREATE TABLE parent_usage_new (
     companion_id TEXT NOT NULL,
     day TEXT NOT NULL,
     used_ms INTEGER NOT NULL,
     PRIMARY KEY (companion_id, day)
   );
   INSERT INTO parent_usage_new (companion_id, day, used_ms)
   SELECT COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = pu.device_id), pu.device_id),
          pu.day, pu.used_ms
   FROM parent_usage pu;
   DROP TABLE parent_usage;
   ALTER TABLE parent_usage_new RENAME TO parent_usage;

   CREATE TABLE tunnels_new (
     companion_id TEXT PRIMARY KEY,
     route_name TEXT NOT NULL,
     url TEXT NOT NULL,
     local_port INTEGER NOT NULL,
     scoped_token TEXT NOT NULL,
     profile TEXT NOT NULL,
     created_at TEXT NOT NULL
   );
   INSERT INTO tunnels_new (companion_id, route_name, url, local_port, scoped_token, profile, created_at)
   SELECT COALESCE((SELECT m.companion_id FROM id_map m WHERE m.old_id = tu.device_id), tu.device_id),
          tu.route_name, tu.url, tu.local_port, tu.scoped_token, tu.profile, tu.created_at
   FROM tunnels tu;
   DROP TABLE tunnels;
   ALTER TABLE tunnels_new RENAME TO tunnels;

   UPDATE memory
   SET scope = 'companion:' || (SELECT m.companion_id FROM id_map m WHERE m.old_id = memory.scope)
   WHERE EXISTS (SELECT 1 FROM id_map m WHERE m.old_id = memory.scope);

   DROP TABLE id_map;
   COMMIT;
   PRAGMA foreign_keys = ON;
   PRAGMA integrity_check;
   ```
3. Run `./scripts/setup-host.sh --gpu-index 0`.
4. Build firmware `0.11.0` for `vocat` and `box3` and OTA each board (`lark ota`).

### Verification
- `GET /health` reports `"ok": true`.
- `GET /api/companions` lists 8 companions (4 with terminals, 4 dormant).
- `GET /api/terminals` lists `vocat-33d8`, `vocat-2cac`, `box3-b324`, and `web-8458606f`.

---

## 2026-11-22 — Unified `lark` Binary, `lark.service`, `lark.env`, and Env/Kconfig Rename

### Affects
- Host / Config / Systemd / Firmware

### Breaking Changes
- **Single binary**: `lark-bridge` and `lark-eval` are replaced by `~/.local/share/lark/lark` with subcommands `serve`, `eval`, `ota`, and `mcp`. `scripts/push-ota.sh` is removed in favor of `lark ota`.
- **Systemd**: `lark-bridge.service` is replaced by `lark.service` (`ExecStart=.../lark serve`). `lark-opencode.service` reads `~/.config/lark/lark.env`.
- **Config & env vars**: `~/.config/lark/bridge.env` moves to `~/.config/lark/lark.env`. `LARK_BRIDGE_TOKEN`, `LARK_BRIDGE_HOST`, and `LARK_BRIDGE_PORT` are renamed to `LARK_TOKEN`, `LARK_HOST`, and `LARK_PORT`.
- **Firmware Kconfig**: `CONFIG_LARK_BRIDGE_URL` and `CONFIG_LARK_BRIDGE_TOKEN` are renamed to `CONFIG_LARK_URL` and `CONFIG_LARK_TOKEN` (`0.10.2`).

### How to Detect
- `systemctl --user is-active lark-bridge.service` is `active`, `~/.config/lark/bridge.env` exists, or `~/.local/share/lark/opencode/config/opencode/opencode.jsonc` references `lark-bridge`.

### Upgrade Steps
1. Back up the database and env file, then stop and disable `lark-bridge.service` and `lark-opencode.service`:
   ```sh
   mkdir -p ~/.local/share/lark/backups
   [ -f ~/.local/share/lark/lark.db ] && sqlite3 ~/.local/share/lark/lark.db ".backup '$HOME/.local/share/lark/backups/pre-lark-cli.db'"
   systemctl --user disable --now lark-bridge.service 2>/dev/null || true
   rm -f ~/.config/systemd/user/lark-bridge.service ~/.local/share/lark/lark-bridge
   ```
2. Rename `bridge.env` → `lark.env` and update its keys:
   ```sh
   cp -a ~/.config/lark/bridge.env ~/.local/share/lark/backups/bridge.env.bak
   mv ~/.config/lark/bridge.env ~/.config/lark/lark.env
   sed -i 's/^LARK_BRIDGE_TOKEN=/LARK_TOKEN=/; s/^LARK_BRIDGE_HOST=/LARK_HOST=/; s/^LARK_BRIDGE_PORT=/LARK_PORT=/' ~/.config/lark/lark.env
   ```
3. Update the MCP command in `~/.local/share/lark/opencode/config/opencode/opencode.jsonc` from `.../lark-bridge` to `.../lark`.
4. Update `firmware/sdkconfig.local` (`CONFIG_LARK_BRIDGE_URL=` → `CONFIG_LARK_URL=`, `CONFIG_LARK_BRIDGE_TOKEN=` → `CONFIG_LARK_TOKEN=`).
5. Run `./scripts/setup-host.sh` (pass `--gpu-index <N>` on multi-GPU hosts).
6. Rebuild firmware (`./scripts/build.sh` for `vocat` and `box3`) and OTA devices with `~/.local/share/lark/lark ota --no-build [--board box3] <ip>`.

### Verification
- `systemctl --user is-active lark.service lark-opencode.service lark-opencode-lan.socket` reports `active`.
- `curl -fsS http://127.0.0.1:8765/health` reports `"ok": true`.
- `GET /api/devices` (with `X-Lark-Token`) shows all devices online on `0.10.2`.

---

## 2026-11-22 — Native Binary Device Protocol (TCP + UDP/RTP on `:8766`)

### Affects
- Bridge (`LARK_DEVICE_PORT`, default `8766`) and `bridge.env` / `lark-bridge.service`
- ESP32 firmware (`vocat` and `box3`) and `scripts/push-ota.sh`

### Breaking Changes
- **Hardware transport**: Firmware replaces WebSocket + cJSON (`espressif/esp_websocket_client`) with a packed binary TCP control/frame/TTS protocol and 20 ms 16 kHz PCM over UDP/RTP on `LARK_DEVICE_PORT` (default `8766`, `0` disables). Browser clients continue using `/ws` WebSocket.
- **Device `/status` field**: Firmware `/status` now reports `connected` instead of `ws_connected` (updated in `scripts/push-ota.sh`).

### Upgrade Steps
1. Ensure `LARK_DEVICE_PORT=8766` is allowed on the LAN firewall (TCP + UDP) and rebuild/restart `lark-bridge.service`.
2. Rebuild and flash/OTA firmware for `vocat` and `box3` (`./scripts/build.sh` / `./scripts/push-ota.sh`).

#### Devices on 0.9.5 (OTA)
Devices running legacy `0.9.5` built before `f48ed64` only accept `echoear` / `echoear_box3` project names, expect `X-EchoEar-Token` on `POST /ota/upload`, and cannot authenticate WebSocket sessions to the new bridge (so rollback would cancel a bridge-gated OTA). Upgrade them via a two-hop OTA through a throwaway worktree at `f48ed64`:
1. **Rename `device_id` in `lark.db`** (while `lark-bridge.service` is stopped, after backing up `lark.db`) for `esp-vocat` devices (`echoear-<mac4>` → `lark-<mac4>`; `box3-<mac4>` stays unchanged):
   ```sql
   BEGIN IMMEDIATE;
   -- Repeat for each table with a device_id column (devices, companions, voices, device_prefs,
   -- messages, sessions, parent_settings, parent_usage, timers, build_jobs, tunnels):
   UPDATE devices SET device_id = 'lark-<mac4>' WHERE device_id = 'echoear-<mac4>';
   -- And memory scopes:
   UPDATE memory SET scope = 'lark-<mac4>' WHERE scope = 'echoear-<mac4>';
   COMMIT;
   ```
2. **Build a throwaway stepping stone** from `f48ed64` (`git worktree add --detach /tmp/opencode/lark-step f48ed64`):
   - Keep `project(echoear)` / `project(echoear_box3)` so `0.9.5` accepts it; `f48ed64`'s `matching_board()` accepts `lark` / `lark_box3` on the next hop.
   - Set `PROJECT_VER` to `0.9.6-step` in `firmware/CMakeLists.txt` and `firmware/boards/box3/CMakeLists.txt`.
   - In `firmware/main/ota_server.c` (`ota_check_boot`), call `esp_ota_mark_app_valid_cancel_rollback()` once `wifi_get_ip(ip)` is non-`"0.0.0.0"` instead of waiting for `ws_client_connected()`.
   - Copy `firmware/sdkconfig.local` with `CONFIG_LARK_` → `CONFIG_ECHOEAR_` and build `vocat` and `box3`.
3. **Two-hop OTA per device** using `X-EchoEar-Token` on `POST http://<ip>/ota/upload`:
   - Hop 1: upload `echoear.bin` / `echoear_box3.bin` (`0.9.6-step`); poll `/status` until `running_partition` flips and `ota_pending` is `false`.
   - Hop 2: upload `lark.bin` / `lark_box3.bin` (`0.10.0`, still using `X-EchoEar-Token`); poll `/status` until `firmware == "0.10.0"`, `connected == true`, and `ota_pending == false`.
   - Remove the throwaway worktree (`git worktree remove --force /tmp/opencode/lark-step`).

---

## 2026-11-22 — EchoEar → Lark Rename, Compat-Shim Removal, and Companion Unification (`e95476d`, `f4a5912`, `206da8c`)

Config/data moved from `~/.config/echoear` and `~/.local/share/echoear` to `~/.config/lark` and `~/.local/share/lark`; env vars `ECHOEAR_*` → `LARK_*`; systemd units `echoear-*` → `lark-*`; OpenCode agent `echoear` → `lark`; tables `cats`/`saved_cats` → `companions`/`saved_companions`; `messages.role` `'cat'` → `'companion'`; routes `/api/cats*` → `/api/companions*`.

1. Stop services, back up DB, and migrate dirs/units/env:
   ```sh
   systemctl --user stop echoear-bridge.service echoear-opencode.service echoear-opencode-lan.socket lark-bridge.service lark-opencode.service lark-opencode-lan.socket 2>/dev/null || true
   mkdir -p ~/.config/lark ~/.local/share/lark/backups
   [ -f ~/.local/share/echoear/echoear.db ] && sqlite3 ~/.local/share/echoear/echoear.db ".backup '$HOME/.local/share/lark/backups/pre-upgrade.db'"
   [ -f ~/.local/share/lark/lark.db ] && sqlite3 ~/.local/share/lark/lark.db ".backup '$HOME/.local/share/lark/backups/pre-upgrade.db'"
   [ -d ~/.config/echoear ] && cp -a ~/.config/echoear/. ~/.config/lark/
   [ -d ~/.local/share/echoear ] && cp -a ~/.local/share/echoear/. ~/.local/share/lark/
   [ -f ~/.local/share/lark/echoear.db ] && [ ! -f ~/.local/share/lark/lark.db ] && mv ~/.local/share/lark/echoear.db ~/.local/share/lark/lark.db
   rm -rf ~/.local/share/lark/opencode/config/opencode/opencode.jsonc ~/.local/share/lark/opencode/config/opencode/agents/echoear* ~/.local/share/lark/opencode/config/opencode/echoear-model-router
   [ -f ~/.config/lark/bridge.env ] && { sed -i 's/^ECHOEAR_/LARK_/; s|/echoear|/lark|g' ~/.config/lark/bridge.env; grep -q '^LARK_DEVICE_PORT=' ~/.config/lark/bridge.env || echo 'LARK_DEVICE_PORT=8766' >> ~/.config/lark/bridge.env; }
   systemctl --user disable --now echoear-bridge.service echoear-opencode.service echoear-opencode-lan.socket echoear-opencode-lan.service 2>/dev/null || true
   ```

2. Migrate `~/.local/share/lark/lark.db` (run only statements for legacy tables that exist):
   ```sql
   PRAGMA foreign_keys = OFF;
   BEGIN IMMEDIATE;
   CREATE TABLE IF NOT EXISTS companions (device_id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '');
   CREATE TABLE IF NOT EXISTS saved_companions (slug TEXT PRIMARY KEY, name TEXT NOT NULL, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '');
   INSERT OR IGNORE INTO companions (device_id, name, configured, face_bg, mark_color, accent_color, personality, updated_at, face_svg)
   SELECT device_id, name, COALESCE(configured, 0), face_bg, mark_color, accent_color, COALESCE(personality, ''), updated_at, COALESCE(face_svg, '') FROM cats;
   INSERT OR IGNORE INTO saved_companions (slug, name, face_bg, mark_color, accent_color, personality, voice, updated_at, face_svg)
   SELECT slug, name, face_bg, mark_color, accent_color, COALESCE(personality, ''), COALESCE(voice, ''), updated_at, COALESCE(face_svg, '') FROM saved_cats;
   UPDATE messages SET role = 'companion' WHERE role = 'cat';
   CREATE TABLE virtual_clients_new (id TEXT PRIMARY KEY, character TEXT NOT NULL, voice TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
   INSERT INTO virtual_clients_new (id, character, voice, created_at) SELECT id, character, COALESCE(voice, ''), created_at FROM virtual_clients;
   DROP TABLE virtual_clients;
   ALTER TABLE virtual_clients_new RENAME TO virtual_clients;
   DROP TABLE IF EXISTS sessions;
   CREATE TABLE sessions (device_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL, contract_hash TEXT NOT NULL DEFAULT '', context_after_ms INTEGER NOT NULL DEFAULT 0);
   DELETE FROM meta WHERE key LIKE 'migrated_%' OR key IN ('schema_version', 'history_backfill', 'mcp_schema_hash', 'mcp_contract_refresh_progress');
   DROP TABLE IF EXISTS cats;
   DROP TABLE IF EXISTS saved_cats;
   DROP TABLE IF EXISTS chat_events;
   DROP TABLE IF EXISTS assets;
   DROP TABLE IF EXISTS schema_version;
   DROP TABLE IF EXISTS history_backfill;
   COMMIT;
   PRAGMA integrity_check;
   ```

3. Reinstall host services (`./scripts/setup-host.sh`), reset each device's OpenCode session without erasing history (`curl -fsS -X POST -H "X-Lark-Token: $TOKEN" -H "Content-Type: application/json" -d '{"archive_history":false}' "http://127.0.0.1:8765/api/devices/{id}/reset"`), re-enter `lark-token` in `/app`, and USB-reflash firmware once (or use the `0.9.6-step` two-hop OTA above for `0.9.5` devices).

---

## 2026-11-22 — Native SQLite WAL and Split Reader/Writer Pools

Replaced `tursogo` with tuned native SQLite WAL. `LARK_TURSO_*` is no longer read:
```sh
sed -i '/^LARK_TURSO_/d' ~/.config/lark/bridge.env
systemctl --user restart lark-bridge.service
sqlite3 ~/.local/share/lark/bridge.db "PRAGMA journal_mode;" # expect: wal
```

---

## 2026-11-22 — Remove Photo Uploads, Ente Photo Library, and ML Photo Indexing

Removed `/api/photos/*`, `/api/devices/{id}/photo`, `/api/library*`, `photos` table, `photo_index.db`, `photo-models`, and `*-ente-photos.{service,timer}`:
```sh
systemctl --user disable --now lark-ente-photos.timer lark-ente-photos.service echoear-ente-photos.timer echoear-ente-photos.service 2>/dev/null || true
rm -f ~/.config/systemd/user/lark-ente-photos.{service,timer} ~/.config/systemd/user/echoear-ente-photos.{service,timer}
systemctl --user daemon-reload
systemctl --user stop lark-bridge.service
sqlite3 ~/.local/share/lark/bridge.db "DROP TABLE IF EXISTS photos;"
rm -rf ~/.local/share/lark/photo_index.db* ~/.local/share/lark/blobs/photos ~/.local/share/lark/photo-models \
       ~/.local/share/lark/bin/ente* ~/.local/share/lark/ente-cli ~/.local/share/lark/ente-export ~/.local/share/lark/ente-photos.*
systemctl --user start lark-bridge.service
```
