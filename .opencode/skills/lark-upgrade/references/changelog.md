# Lark Upgrade Changelog

Newest first; apply bottom-up (oldest entry to the newest) from the installation's starting state. Always back up `lark.db` and `lark.env` before mutating anything.

---

## 2026-11-24 — OpenCode 2.3.2 → 2.4.1

### Affects
- Host: the OpenCode binary (`~/.opencode/bin/opencode`, shared with the user's interactive OpenCode)

### Breaking Changes
- None for Lark. `session.hook("context"|"compaction"|"experimental.model.resolve")` is unchanged; both Lark plugins load `active`.
- Plugins load asynchronously: `/api/model` and `/api/plugin` can be empty for a few seconds after start. Lark refreshes on `model.updated`/`agent.updated`.
- `experimental_image_generate` is renamed `generate_image`. Lark does not use it.
- The experimental instruction-entry route is unchanged (`PUT /api/experimental/session/{sid}/instructions/entries/lark.companion` returns 204).

### How to Detect
- `GET http://127.0.0.1:49375/api/info` reports `2.3.2` while `~/.opencode/bin/opencode --version` reports 2.4.1: the service has not restarted onto the installed binary.

### Upgrade Steps
1. `cp -a ~/.opencode/bin/opencode ~/.local/share/opencode-bin-backup/opencode-$(~/.opencode/bin/opencode --version)`
2. `~/.opencode/bin/opencode upgrade`
3. `systemctl --user restart lark-opencode.service lark.service`

### Verification
- `lark status` and `lark doctor` pass.
- `GET /api/plugin` lists `lark.model-fallback` and `lark.context` as `active`.
- A `lark sim` voice turn returns audio; `/api/agent` lists `deep` on `opencode/glm-5.3#high`.

---

## 2026-11-24 — Firmware 0.21.0: lossless TTS acks, 12 s TTS buffer, reconnect and telemetry fixes

### Affects
- Firmware (`esp-vocat`, `esp-box-3`) / Server telemetry consumers

### Breaking Changes
- Firmware `0.21.0`. Devices on `0.20.0` keep working; this is an OTA-only upgrade.
- `TTS_ACK` is now delivered through a dedicated pending slot and retried until sent or its stream is replaced. A completed ack is never retracted by a late abort, and `TTS_END` always closes an accepted stream, so the server receives exactly one completion or abort per stream it starts. A wrong token or protocol version now stops reconnect attempts until the device reboots; it no longer loops at 1 Hz.
- `/status` gains `tts_gap_ms`: cumulative underrun duration in milliseconds for the current TTS stream, reported alongside `tts_first_us` and `audio_underruns` (which keeps counting gaps). `tts_samples` now advances as PCM is written to the codec, not only when the stream ends, and resets at each TTS start.
- `audio_drops` now also counts TTS ring drops (backpressure timeout) and failed recording datagrams, not only input-ring drops. A recording is cancelled only after 50 consecutive UDP send failures (about 1 s); single failures are dropped and counted.

### How to Detect
```sh
~/.local/share/lark/lark device <terminal> status | jq -r '.firmware, has("tts_gap_ms")'
```

### Upgrade Steps
1. No host, config or SQLite step.
2. OTA each terminal from a checkout of this commit: `lark ota <ip>` (vocat), `lark ota --board box3 <ip>`. The `/status` board check refuses a mismatched image.

### Verification
- `/status` reports `"firmware":"0.21.0"` and includes `tts_gap_ms`.
- During a long TTS reply (10+ sentences) `tts_samples` rises before the reply ends, `tts_gap_ms` stays small, and `audio_underruns` does not increase; the reply finishes without an aborted ack.
- The pairing screen stays at brightness 100 after a touch or a TTS wake while linked; an offline panel still dims after two idle minutes.
- A scratch build with a wrong `CONFIG_LARK_TOKEN` logs `Bridge rejected connection` once and stops reconnecting instead of retrying each second.

---

## 2026-11-23 — Context layout: session identity entry, one live state line

### Affects
- Agent prompt / Config (`opencode.jsonc`, `./lark-context` plugin) / Lark API & MCP / OpenCode sessions

### Breaking Changes
- Turn headers no longer begin with `You are <name> (<id>).`; they start with `Speaker: …` or `Event: timer`.
- Companion identity is the session instruction entry `lark.companion` (`{id,name,personality}`), written with `PUT /api/experimental/session/<sid>/instructions/entries/lark.companion` at session create and re-sent when a name or personality edit lands (the update appends a system row and reaches the model a step late); session create metadata is `{"lark":{"companion":"<id>"}}`. The route is experimental and must be re-checked on OpenCode upgrades.
- `[library: …]` / `[story: …]` hints left the per-turn prompt; that policy now lives in the MCP `instructions` (system prompt).
- The `lark.context` OpenCode plugin strips stored `[state: …]` blocks from model requests, keeping only the current user message (and none during compaction). Stored history still contains them.
- The OpenCode contract hash changes, so existing sessions roll over on the next user turn; no manual reset is needed.

### How to Detect
```sh
jq -r '.plugins[].package' ~/.local/share/lark/opencode/config/opencode/opencode.jsonc | grep -qx ./lark-context
```

### Upgrade Steps
1. Back up `lark.db`, `lark.env`, and `opencode.jsonc`, then run `./scripts/setup-host.sh --gpu-index 0`. It installs `config/opencode/lark-context/index.js` and appends `{"package":"./lark-context"}` without touching model selections; restart `lark-opencode.service` if setup did not reload it.
2. No SQLite or firmware step is needed.

### Verification
- A new turn's prompt starts with `[Speaker: …]` or `[Event: timer]` and ends with exactly one `[state: …]` line.
- `GET /api/experimental/session/<sid>/instructions/entries` lists `lark.companion` with the companion's id and name; editing the name or personality refreshes that entry on the bound session; `GET /api/session/<sid>/context` still shows historical `[state:` rows.
- `GET /health` reports `"ok": true`; plugin tests (`node --test config/opencode/*.test.mjs`) pass.

---

## 2026-11-23 — Background model loading, `lark deploy`, fast firmware builds

### Affects
- Server / scripts / firmware build workflow

### Breaking Changes
- None. `/health` gains `models` (per-model `loading|ready|error`) and
  `models_ready`; the existing `stt`, `tts` and `smart_turn` booleans are
  unchanged and become true once their model is ready.
- `scripts/build.sh` skips a board whose sources and `sdkconfig*` are unchanged
  since its last successful build; use `--force` to rebuild.

### How to Detect
- `GET /health` lacks `models` or `models_ready`, or `lark status` prints no
  `Models:` line.

### Upgrade Steps
1. Back up `lark.db` and `lark.env`.
2. Install the new server with `./scripts/setup-host.sh --gpu-index 0` once.
   Later server updates can use `~/.local/share/lark/lark deploy` from the
   checkout.
3. No firmware or database step is needed.

### Verification
- Immediately after a restart, `lark status` prints `Models:` with each model
  `loading` and then `ready`; `/health` has `models` and `models_ready`.
- A turn sent while models are still loading waits for them instead of failing.
- `lark deploy` prints build, install, restart, serving, models and terminals
  timings and exits 0 with every previously connected terminal back online.
- `./scripts/build.sh vocat` twice: the second prints `up to date` in under a
  second. `--force` rebuilds; `--watch` rebuilds when sources change.

---

## 2026-11-23 — Manual Volume Only

### Affects
- Server / SQLite (`~/.local/share/lark/lark.db`) / Lark API & MCP / Web (`/remote`) / On-device Volume menu / Agent prompt

### Breaking Changes
- Automatic volume is gone. Each terminal has one manual level; the server no longer derives it from ambient noise or time.
- `terminal_prefs` drops the retired automatic-volume column and renames `manual_volume` to `volume`. The server renames first, then drops the retired column at startup, guarded.
- The retired automatic-volume `patch` argument is rejected. `POST /api/terminals/{id}/volume` and `/remote/api/volume` take `{"volume":0..100}` only; `PATCH /api/terminals/{id}` ignores `auto`.
- `state` terminal volume objects, `/api/terminals`, and diagnostics no longer carry the automatic-volume flag; diagnostics loses `ambient_rms`.
- The on-device Volume menu has Quieter/Louder only; the Mode toggle is gone.
- `VOLUME` keeps its 5-byte payload with the auto byte retired (always 0), so firmware `0.20.0` keeps working without an OTA.

### How to Detect
```sh
sqlite3 ~/.local/share/lark/lark.db "SELECT group_concat(name) FROM pragma_table_info('terminal_prefs') WHERE name IN ('auto_volume','manual_volume','volume');"
curl -s -H "X-Lark-Token: $LARK_TOKEN" http://127.0.0.1:8765/api/terminals | grep -o auto_volume
```
`volume` alone means upgraded. A retired column name, or any hit from `curl`, means the upgrade is pending.

### Upgrade Steps
1. Stop the service, back up, and apply the schema step (the server also does it at startup). The guard makes a re-run safe:
   ```sh
   systemctl --user stop lark.service
   db=~/.local/share/lark/lark.db
   sqlite3 "$db" ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   has() { sqlite3 "$db" "SELECT count(*) FROM pragma_table_info('terminal_prefs') WHERE name='$1';"; }
   if [ "$(has manual_volume)" = 1 ]; then
     sqlite3 "$db" "ALTER TABLE terminal_prefs RENAME COLUMN manual_volume TO volume;"
   fi
   if [ "$(has auto_volume)" = 1 ]; then
     sqlite3 "$db" "ALTER TABLE terminal_prefs DROP COLUMN auto_volume;"
   fi
   ```
2. Run `./scripts/setup-host.sh --gpu-index 0` to rebuild and restart.
3. No firmware change is needed on `0.20.0`.

### Verification
- `GET /api/terminals` shows one `volume` per terminal and no automatic-volume flag.
- `sqlite3 ... "SELECT terminal_id,volume FROM terminal_prefs;"` keeps the former manual levels.
- The device Volume menu lists Quieter and Louder only; a change persists to `volume` and survives a reconnect.
- `patch({terminal:{volume:40}})` and the `/remote` slider both apply immediately; the retired automatic-volume argument is rejected as unknown.

---

## 2026-11-23 — Remote microphone capture

### Affects
- Server / Lark API & MCP / Web (`/remote`) / Local debug captures

### Breaking Changes
- None. This adds scoped routes and a Diagnostics tab; no schema, wire, or config change.

### How to Detect
- None; additive. `GET /remote/api/captures` returns 404 on an older server.

### Upgrade Steps
1. Back up `lark.db` and `lark.env`, then run `./scripts/setup-host.sh --gpu-index 0` to rebuild and restart `lark.service`.
2. No firmware or database step is needed.

### Verification
- `/remote` has five tabs; Diagnostics shows 15/30/60 s duration buttons, one Start button, and the privacy line.
- Start creates `~/.local/share/lark/debug/capture-<companion>-<stamp>.wav` (`0600` in a `0700` directory) and progress arrives as `capture` SSE events.
- A second start while one runs returns `409`; Play streams the WAV with range support, Transcribe writes the `.txt` sidecar, and Delete removes both files.
- `lark capture <companion> <seconds>` still records through the same path and prints the WAV path and duration.

---

## 2026-11-23 — Listening on/off and silence-driven endpoints

### Affects
- Server / SQLite (`~/.local/share/lark/lark.db`) / Lark API & MCP / Web (`/remote`) / Agent prompt

### Breaking Changes
- The old per-window duration column (0–300 s) is gone. Listening is a boolean on/off setting; the window length is one server constant (30 s).
- The retired duration `patch` argument is rejected. Use `patch.companion.listening: true|false`. `/remote/api/listen` takes `{"listening":true|false}`; the six duration pills and "Off" pill are replaced by one Listen toggle.
- `state` returns `me.companion.listening` instead of the duration; the turn state line says `listen: on` / `listen: off`.
- The server performs the one-time backfill and drop at startup, guarded like other retired columns.

### How to Detect
```sh
sqlite3 ~/.local/share/lark/lark.db "SELECT group_concat(name) FROM pragma_table_info('companions') WHERE name IN ('listen_seconds','listening');"
curl -s -H "X-Lark-Token: $LARK_TOKEN" http://127.0.0.1:8765/api/companions | grep -o '"listen_seconds"'
```
`listening` alone means upgraded. The retired column or JSON key means the upgrade is pending.

### Upgrade Steps
1. Stop the service, back up, and apply the schema step (the server also does it at startup). The guard makes a re-run safe:
   ```sh
   systemctl --user stop lark.service
   db=~/.local/share/lark/lark.db
   sqlite3 "$db" ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   has() { sqlite3 "$db" "SELECT count(*) FROM pragma_table_info('companions') WHERE name='$1';"; }
   if [ "$(has listen_seconds)" = 1 ]; then
     if [ "$(has listening)" = 0 ]; then
       sqlite3 "$db" "ALTER TABLE companions ADD COLUMN listening INTEGER NOT NULL DEFAULT 0;"
     fi
     sqlite3 "$db" "UPDATE companions SET listening=1 WHERE listen_seconds>0;"
     sqlite3 "$db" "ALTER TABLE companions DROP COLUMN listen_seconds;"
   fi
   ```
2. Run `./scripts/setup-host.sh --gpu-index 0` to rebuild and restart.
3. No firmware change is needed on `0.20.0`.

### Verification
- `GET /api/companions` shows boolean `listening` for every companion and no duration key.
- `sqlite3 ... "SELECT id,listening FROM companions;"` matches the old durations (`>0` becomes `1`).
- `/remote` shows one Listen toggle; no duration pills.
- A window with quiet speech commits within ~1.6 s of the last voiced frame; noise-only windows close at 30 s without a turn.

---

## 2026-11-23 — Companion Visibility, Reply Routing and Patch Defaults

### Affects
- Server, agent prompt, Delta Chat accounts, diagnostics

### Breaking Changes
- None. No schema or wire change; companion IDs, forms, voices and accounts are preserved.

### How to Detect
- None; additive. An affected prompt lists `terminals: none` while an online hardware terminal is attached.

### Upgrade Steps
1. Back up `lark.db` and `lark.env`, then install the new `lark` binary and restart `lark.service` and `lark-opencode.service`.
2. Optional live cleanup: delete unconfigured `New Companion` rows, `*-classic` companions and `sim-*` terminal rows, and remove their Delta Chat accounts. Companion deletion now removes the account through the rpc.
3. Set `LARK_DEBUG_ENDPOINT=1` in `lark.env` only if end-of-turn decisions should be logged.

### Verification
- A turn's prompt lists an attached online terminal (id, board, battery, sleeping, volume) instead of `terminals: none`.
- A `/say` queued behind an in-flight capture does not text the sender; a later capture that answers it does.
- `patch({terminal:{volume:40}})` defaults to the speaking companion and its voice terminal.
- `GET /api/diagnostics` shows `silent:true` and `tool_errors` on affected recent turns.

---

## 2026-11-23 — Server Cleanup: Retired Wire Status, Telemetry Fields, Timer Column

### Affects
- Server / SQLite (`~/.local/share/lark/lark.db`) / Firmware (`0.20.0`)

### Breaking Changes
- The server no longer sends meaningful state sequence numbers, status progress/kind/text, or voice-owner sequence fields. Payload sizes are unchanged so firmware `0.20.0` (which ignores them) and the server can ship independently.
- `lark` no longer fills `battery_mv`, `min_free_heap`, `free_psram`, `running_partition`, display-flush/input-drop/audio-underrun counters, `ws_connected` or `reported_volume` in diagnostics; `/api/terminals` and the diagnostic report lose those keys.
- Unrouted server code was deleted: `/api/memory`, `/api/timers`, the compact endpoint, legacy `/api/devices…` messaging helpers and the WebSocket-era terminal state. None had a route or MCP tool.
- Fresh schemas drop `timers.app_name`; existing databases lose it once at startup. Message rows written before the channel column carry `channel=''` until the backfill below.

### How to Detect
```sh
sqlite3 ~/.local/share/lark/lark.db "SELECT COUNT(*) FROM timers WHERE 1; PRAGMA table_info(timers);" | grep -c app_name
sqlite3 ~/.local/share/lark/lark.db "SELECT COUNT(*) FROM messages WHERE channel='';"
```
Non-zero for either means the upgrade below is pending. Older firmware reports a version below `0.20.0`.

### Upgrade Steps
1. Stop the service and back up the database:
   ```sh
   systemctl --user stop lark.service
   sqlite3 ~/.local/share/lark/lark.db ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   ```
2. Backfill the retired channel column and drop the timer column (the server also drops `app_name` at startup, guarded):
   ```sql
   UPDATE messages SET channel='in_person' WHERE channel='' AND source IN ('voice','typed','tap','text');
   UPDATE messages SET channel='deltachat' WHERE channel='' AND source='deltachat';
   UPDATE messages SET channel='intercom' WHERE channel='' AND source='intercom';
   UPDATE messages SET channel=source WHERE channel='';
   ALTER TABLE timers DROP COLUMN app_name;
   ```
3. Run `./scripts/setup-host.sh --gpu-index 0` to rebuild and restart, then OTA firmware `0.20.0` to every board.

### Verification
- `lark status` reports `ok=true` and all terminals on `0.20.0`.
- The two detection queries return `0`.
- `GET /api/diagnostics` has no `battery_mv`, `free_psram`, `ws_connected` or `reported_volume` keys.

---

## 2026-11-23 — Firmware `0.20.0` Cleanup, Wake and Stop Fixes

### Affects
- Firmware (`0.20.0`), BOX-3 dependency lock

### Breaking Changes
- **Wake race**: the loopback wake socket is drained to EWOULDBLOCK before its pending flag clears, so a request racing the drain can no longer strand input until the next 5 s status tick. A failed wake send clears the flag and retries later; if the wake socket cannot be created, the service loop polls every 50 ms.
- **Pairing brightness**: `POWER` levels 0–100 are accepted, so the pairing QR screen reaches 100%; idle sleep and touch wake still restore the 12/85% UI range.
- **Stop**: aborting speech still writes the BSP's bounded 90 ms TX tail, so the next chime or reply never starts behind stale speech.
- **Memory**: the 320 KiB `/debug/mic` buffer allocates on first upload, and the send-only RTP UDP socket no longer reserves a receive buffer.
- **Dependencies**: BOX-3 builds against `esp-box-3_noglib` 3.2.1 and no longer locks `esp_lvgl_port`/`lvgl`. VoCat keeps `esp_vocat` 1.1.0 because the published `esp_vocat_noglib` 1.1.0 does not compile (an `lv_indev_t` declaration sits outside its LVGL guard).
- **Removed**: `echo_suppress.c` and its host test, `LARK_MSG_MEDIA`, `LARK_MSG_SET_VOLUME`, `LARK_MSG_VOICE_OWNER`, `LARK_HELLO_FLAG_IF_FREE`, `LARK_REJECT_OCCUPIED`, the blocked state, `LARK_TELEMETRY_SLEEPING`, `BOARD_SHAPE`, `BOARD_HAS_DRAG`, the retired audio-call sample constant.
- **Protocol**: terminals stop sending and consuming state sequence numbers. `RESUME.last_seq` is always 0 and the wire layout is unchanged, so 0.19.0 and newer servers both work; `STATUS` is read for its state byte only and may drop text/progress/kind. Server-side seq removal and the 12 s → 30 s sleeping ping backoff follow in a separate push; both sides tolerate the other.
- **Tests**: new wake-drain ordering test and a 120x160 dirty-rectangle commit/strip-copy test on both board geometries.

### How to Detect
- `GET /status` on the board reports a firmware version below `0.20.0`.

### Upgrade Steps
1. `lark ota --board vocat 192.168.1.120`, then `lark ota --board vocat 192.168.1.183`, then `lark ota --board box3 192.168.1.100`.
### Verification
- `GET /status` reports `0.20.0` with a healthy `min_free_heap`; a tap-to-talk turn and a TTS reply both work, wake and Stop behave.

---

## 2026-11-23 — Library Auto-Provisioning by Disk Budget


### Affects
- Host / `~/.local/share/lark/library/`, `lark.service`, `setup-host.sh`

### Breaking Changes
- The installer no longer accepts a library flag; the server provisions the content library itself. Manual `lark library --download --build --force` remains.
- Provisioning decides by free disk, pending packs smallest first: Simple English Wikipedia, then Project Gutenberg, then full English Wikipedia when it fits within a 20% reserve plus 10% margin. There is no fixed 100 GB/140 GB requirement.

### How to Detect
```sh
./scripts/setup-host.sh --library  # must fail with an unknown option
```
- `lark status` prints no `Library (...)` section.

### Upgrade Steps
1. Run `./scripts/setup-host.sh --gpu-index 0`. It restarts `lark.service`, which plans against the current disk.
2. Check `~/.local/share/lark/lark status`; packs already installed stay installed and an in-progress build is reported as `building`.
3. Expect the existing enwiki dump to disappear after its index is present (`df` gains about 24 GB).

### Verification
- `lark status` reports `installed`, `building (pct)`, `pending`, or `skipped (space)` per pack plus free/total/budget.
- `lark library --plan` prints the same decision as JSON.

---

## 2026-11-23 — Remote Controls Replace the Webxdc Panel

### Affects
- Server / Delta Chat / Host (`opentunnel`)

### Breaking Changes
- Deleted the old Delta Chat panel implementation: its status-update handler, versioning, story player, and command. Remaining panel messages are inert.
- `/remote` replaces it: one scoped `https://<opentunnel-route>/remote?token=...` link per verified contact. Each request mints a fresh token and invalidates the previous one; `/remote off` revokes it.
- Earlier panel feature entries are superseded by this one and kept for history only.

### How to Detect
- `GET /api/remote/tunnels` is empty before the first `/remote`.
- `/remote` gets the "not configured" reply when `opentunnel` is missing.

### Upgrade Steps
1. Install `opentunnel` from https://opentunnel.xyz if it is not already available (or set `LARK_OPENTUNNEL_BIN`).
2. Restart `lark.service`.
3. Send `/remote` from a verified contact to mint the first link. Old panel messages can be deleted manually.

### Verification
- The `/remote` link loads the companion page, shows live state, and accepts Say/listen/volume/story/history/voice commands.
- `lark tunnels` lists the companion's route; `/remote off` removes it and the token.

---

## 2026-11-23 — Offline Content Library (superseded)

### Affects
- Host / `~/.local/share/lark/library/`, `lark.service`, `lark-opencode.service`, `check.sh`, `setup-host.sh`

### Breaking Changes
- Superseded by Library Auto-Provisioning above: the installer's library flag and the fixed free-space check are gone; the planner decides what fits. The 100 GB/140 GiB figures below are historical measurements, not requirements.
- Adds `library` tool, FTS5 indexes and a prompt hint; existing installations gain them after provisioning.
- `enwiki.db` is renamed into place only when its FTS5 build finishes, so search uses it without a restart.

### How to Detect
- `~/.local/share/lark/lark library --status` reports no installed packs.
- The installer rejects the removed library flag.

### Upgrade Steps
1. Run `./scripts/setup-host.sh --gpu-index 0` and let the server provision what the disk allows.
2. Verify with `~/.local/share/lark/lark library --status`; index builds continue in the background and log to `library/setup.log`.

### Verification
- `lark status` lists the library packs; `lark library --plan` shows the decision.
- `lark_*` MCP `library` search returns snippets for a known article once `enwiki.db` is installed.

---

## 2026-11-23 — Delta Chat DB Reclaim After Panel Echo

### Affects
- Host / SQLite (`~/.local/share/lark/deltachat/`)

### Breaking Changes
- None; one-time housekeeping. Superseded by the wire-log purge below.

### How to Detect
```sh
DB=~/.local/share/lark/deltachat/216ef3f6-f7b0-4e73-9cdc-c952af9ec643/dc.db
sqlite3 "file:$DB?mode=ro" "PRAGMA freelist_count; PRAGMA page_count;"
```
A large freelist after the panel echo bug means reclaiming space is still pending.

### Upgrade Steps
1. Stop `lark.service`, copy `dc.db`/`dc.db-wal` aside on the same filesystem, run `VACUUM` and `PRAGMA integrity_check` via Python `sqlite3` (Delta Chat has no vacuum RPC), then restart.

### Verification
- `PRAGMA integrity_check` returns `ok` and the DB file size drops.

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
1. Stop `lark.service`. Reflink-backup the account dir (`cp -a --reflink=always`).
2. Delete the stale wire log and its queue, then vacuum:
   ```sql
   DELETE FROM smtp WHERE msg_id IN (SELECT id FROM msgs WHERE param LIKE '%S=30%');
   DELETE FROM smtp WHERE msg_id NOT IN (SELECT id FROM msgs);
   DELETE FROM msgs WHERE param LIKE '%S=30%';
   VACUUM;
   PRAGMA integrity_check;
   ```
   The `delete_messages` RPC only trashes rows (zeroed tombstones), so direct SQL is required to reclaim space.
3. Prune unreferenced accounts with a one-off `RemoveAccount` client (`DC_ACCOUNTS_PATH` must point at `~/.local/share/lark/deltachat`, service stopped). Keep the `dc_account_id`s still present in `companions`; remove QA companions first (`lark companions rm <id>`), then restart.

### Verification
- `PRAGMA integrity_check` returns `ok`; the `S=30` count is single-digit and `smtp` drains to 0.
- `lark status` lists the household companions.

---

## 2026-11-23 — Silent Story Opens

### Affects
- Server / Agents

### Breaking Changes
- None; additive. Opening a story from the device menu, Memories → Reading, or a remote Open/Start over shows the passage silently — no TTS and nothing queued. Voice choices still narrate the next passage.

### How to Detect
- A menu-opened story narrates its first passage on the previous release.

### Upgrade Steps
1. Restart `lark.service`.

### Verification
- Open a story from the menu: the passage appears with no speech; choosing a voice option narrates the next passage.

---

## 2026-11-23 — OpenCode Zen Gateway

### Affects
- Config / Lark Server / Agents

### Breaking Changes
- Model ids moved from `opencode-go/…` to `opencode/…`: `lark` `deepseek-v4.1-flash#low`, `deep` `glm-5.3#high`, fallback `glm-5.3-flash#low`. `setup-opencode-config.py` rewrites only an untouched generated config; custom model choices are kept.
- The dedicated instance imports `opencode` credentials once via `setup-host.sh` and marks `state/imported-provider-opencode`.
- Measured (20 interleaved warm turns): step start p50/p95 1,087/1,448 ms vs 1,058/1,523 ms; first text delta 1,087/1,448 vs 1,066/1,523 ms; 5/5 hard-prompt rubric.

### How to Detect
- `grep opencode-go ~/.local/share/lark/opencode/config/opencode/opencode.jsonc` still matches.
- `state/imported-provider-opencode` is absent from the OpenCode state dir.

### Upgrade Steps
1. Re-run `./scripts/setup-host.sh` (migrates the generated config and imports Zen auth if missing), or edit `opencode.jsonc` to the three ids above and restart `lark-opencode.service`.

### Verification
- `lark-opencode.service` is active and `GET /api/diagnostics` reports a successful model call.

## 2026-11-23 — Webxdc Panel v4 and v3 (superseded)

### Affects
- Server / Webxdc

### Breaking Changes
- Superseded by "Remote Controls Replace the Webxdc Panel". Both panel releases were deleted with the webxdc code; no upgrade action remains.

### How to Detect
- None. The panel no longer exists.

### Upgrade Steps
1. None; follow the Remote Controls entry.

### Verification
- `/remote` works; `/panel` does not.

---

## 2026-11-22 — Semantic End-of-Turn (Smart Turn v3.2) & Malloc Arena Cap

### Affects
- Host (`stt-models`, `lark.service`) / Lark Server

### Breaking Changes
- Additive: in-process Whisper log-mel plus `smart-turn-v3.2-cpu.onnx` (via `onnxruntime_go`) for ~200 ms pause detection, falling back to the tuned hangover.
- `MALLOC_ARENA_MAX=2` and `mallopt(M_ARENA_MAX, 2)` bound native arena fragmentation.

### How to Detect
- `~/.local/share/lark/stt-models/smart-turn-v3.2-cpu.onnx` is missing.
- `systemctl --user show lark.service -p Environment` lacks `MALLOC_ARENA_MAX=2`.

### Upgrade Steps
1. Re-run `./scripts/setup-host.sh` (or `--models-only` + rebuild) to download the model and restart `lark.service`.

### Verification
- `/api/diagnostics` shows smart-turn latency samples and the service RSS plateaus.

---

## 2026-11-22 — Interactive Stories (Twee 3), Feature Cuts & Firmware (`0.18.0`) (superseded)

**Superseded — no action.** The apps, games, build-job, weather, and wake-word
features described below were deleted. The story tables and tool remain; their
later changes appear in newer entries. Do not run the app conversions or the
destructive SQL on a current installation.

### Affects
- Host / Config / SQLite / Lark Server & MCP / Agents / Webxdc / Firmware (`0.18.0`)

### Breaking Changes
- **Removed features**:
  - Removed JS apps/games (`goja`, templates, `apps` table, `app` MCP tool, `app:*` and `game:*` memory scopes, `timer.app`, `Games` menu).
  - Removed background build jobs (`build_jobs` table, `build` MCP tool, `lark-builder` agent).
  - Removed weather service (`LARK_LOCATION`, screen weather tint/badge).
  - Removed leftover `history` drawer, `lark eval` subcommand, and wake-word detection (`espressif/esp-sr`, `WAKE_WORD`, `WAKE_WORD_CTRL`, `LARK_CAP_WAKE_WORD`).
- **Interactive stories (Twee 3 subset)**:
  - Replaced `apps` and `Books` with `stories` and `story_progress` tables, `story` MCP tool (`create`, `update`, `get`, `delete`), `Stories` device menu & choice reader with voice-choice matching and lazy `bg` generation, Delta Chat `/stories`, `/story <name>`, `.twee` attachment import, and webxdc story player.
  - Bumped `dcWelcomeVersion` and `dcPanelVersion` to `"2"`.
- **OpenCode config**: Removed `setup-opencode-config.py` model-migration shim; existing installations on `opencode/gemini-4-hydrogen#low` should update `opencode.jsonc` once.
- **Firmware `0.18.0`**: Removed `espressif/esp-sr` WakeNet and wake-word protocol messages.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db ".tables"` still lists `apps` or `build_jobs`, or lacks `stories` / `story_progress`.
- `grep -q LARK_LOCATION ~/.config/lark/lark.env` succeeds.
- Terminals report firmware `< 0.18.0`.

### Upgrade Steps
1. Stop `lark.service` and back up `lark.db`, `lark.env`, and `opencode.jsonc`:
   ```sh
   systemctl --user stop lark.service
   sqlite3 ~/.local/share/lark/lark.db ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   cp ~/.config/lark/lark.env ~/.config/lark/lark.env.bak.$(date +%Y%m%d-%H%M%S)
   ```
2. Apply SQLite schema & data upgrade:
   ```sql
   CREATE TABLE IF NOT EXISTS stories (id TEXT PRIMARY KEY, title TEXT NOT NULL, twee TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL, passages INTEGER NOT NULL DEFAULT 0);
   CREATE TABLE IF NOT EXISTS story_progress (companion_id TEXT NOT NULL, story_id TEXT NOT NULL, passage TEXT NOT NULL, page INTEGER NOT NULL DEFAULT 0, flags_json TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL, PRIMARY KEY (companion_id, story_id));

   INSERT OR REPLACE INTO stories (id, title, twee, author, updated_at, passages)
   SELECT
     name,
     title,
     CASE
       WHEN length(coalesce(json_extract(data_json, '$.text'), '')) <= 600 THEN
         ':: StoryTitle' || char(10) || title || char(10) || char(10) || ':: Start {"end":true}' || char(10) || coalesce(json_extract(data_json, '$.text'), '') || char(10)
       ELSE
         ':: StoryTitle' || char(10) || title || char(10) || char(10) ||
         ':: Start' || char(10) || substr(json_extract(data_json, '$.text'), 1, 450) || char(10) || char(10) || '[[Next->Part 2]]' || char(10) || char(10) ||
         ':: Part 2 {"end":true}' || char(10) || substr(json_extract(data_json, '$.text'), 451) || char(10)
     END,
     author,
     updated_at,
     CASE WHEN length(coalesce(json_extract(data_json, '$.text'), '')) <= 600 THEN 1 ELSE 2 END
   FROM apps
   WHERE kind = 'book';

   DROP TABLE IF EXISTS build_jobs;
   DROP TABLE IF EXISTS apps;
   DELETE FROM memory WHERE scope LIKE 'app:%' OR scope LIKE 'game:%';
   ```
3. In `~/.local/share/lark/opencode/config/opencode/opencode.jsonc`, remove `agents.lark-builder` if present and ensure `agents.lark.model` is `opencode/claude-haiku-5-5#none` with fallback `opencode/gemini-3.8-flash#low` (if still on the old defaults).
4. Run `./scripts/setup-host.sh --gpu-index 0`.
5. Build and OTA firmware `0.18.0` to all boards (`vocat-33d8` → `vocat-2cac` → `box3-b324`).

### Verification
- `lark status` shows `ok=true` and all terminals online on `0.18.0`.
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, title, passages FROM stories ORDER BY id;"` lists converted stories and no `apps` or `build_jobs` tables.

---

## 2026-11-22 — Delta Chat Welcome Instructions (`dc_welcome` in `meta`)

### Affects
- SQLite (`meta` keys) / Lark Server

### Breaking Changes
- **Welcome & `/help` versioning**: Added `dcWelcomeVersion` (`"1"`), tracked per companion and verified human contact in `meta` (`dc_welcome:<companion_id>:<contact_addr>`).
- **Panel versioning (historical)**: a per-contact panel version was tracked in `meta` (`dc_panel:<companion_id>:<contact_addr>` and `dc_panel_serial:<acc_id>:<msg_id>`) and re-sent on startup or SecureJoin when it changed. The panel was later deleted.
- **Startup & SecureJoin delivery**: On startup (`syncWelcomes`) and on SecureJoin verification (`EventTypeSecurejoinInviterProgress`), companions send the welcome message once per version to verified human contacts (never to companions).

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db "SELECT key, value FROM meta WHERE key LIKE 'dc_welcome:%';"` is empty when no welcomes have been recorded.

### Upgrade Steps
1. Deploy the updated server (`./scripts/setup-host.sh --gpu-index 0`). No schema migration is needed because `meta` already exists.

### Verification
- `journalctl --user -u lark.service` shows startup sync without errors, and existing verified human contacts receive the welcome once (not re-sent on restart).

---

## 2026-11-22 — Strict Stored Voice Validation & Head-Pad Removal (`0.16.0`)

### Affects
- SQLite / Lark Server / Firmware (`0.16.0`)

### Breaking Changes
- **Removed `storedVoice` compat shim**: Legacy voice IDs (such as `bf_emma`) are no longer mapped silently to curated voices. Any unknown non-empty stored voice in `companions.voice` or TTS requests now fails validation with an error.
- **Removed head-pad capability**: Removed `LARK_CAP_HEAD_PADS`, `BOARD_HAS_HEAD_PADS`, `capHeadPads`, and `head_pads` from firmware, binary protocol, and server.
- **Firmware `0.16.0`**: Bumped `PROJECT_VER` to `0.16.0` on VoCat and BOX-3.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, voice FROM companions WHERE voice != '' AND voice NOT IN ('af_heart','bm_fable','bf_lily','bm_george','af_bella','am_puck','bf_alice','bf_isabella');"` returns rows.
- Terminals report firmware `< 0.16.0`.

### Upgrade Steps
1. Ensure all `companions.voice` rows use curated voices (`af_heart`, `bm_fable`, `bf_lily`, `bm_george`, `af_bella`, `am_puck`, `bf_alice`, `bf_isabella`).
2. Run `./scripts/setup-host.sh --gpu-index 0`.
3. Build and OTA firmware `0.16.0` (`vocat-33d8` → `vocat-2cac` → `box3-b324`).

### Verification
- `lark status` shows `ok=true` and all terminals online on `0.16.0`.

---

## 2026-11-22 — Tap-to-Talk, Simplified Menu & Curated Voices

### Affects
- SQLite / Lark Server & MCP / Agents

### Breaking Changes
- **Home & Menu gestures**: Restored tap-anywhere-on-Home to start talking (`listen`); swipe up on Home opens Menu. Touchscreen-only on both VoCat and BOX-3 (no head pads).
- **Simplified Menu**: Reduced main Menu to 6 items (`Talk to…`, `Books`, `Games`, `Memories`, `Voice`, `More`), moved `Volume`, `Pair`, `Unpair`, `Stats` under `More`, removed `History` from the device menu, centered `Back` consistently, and enlarged bottom/nav touch targets to >= 48 physical px.
- **Voice catalog**: Removed `bf_emma` ("too clinical") and restored the curated 8-voice kid-friendly catalog (`af_heart` Heart, `bm_fable` Fable, `bf_lily` Lily, `bm_george` George, `af_bella` Bella, `am_puck` Puck, `bf_alice` Alice, `bf_isabella` Isabella).
- **Mint voice**: Moved Mint from `bf_emma` to `af_heart` ("Heart").

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, voice FROM companions WHERE voice='bf_emma';"` returns rows.

### Upgrade Steps
1. Stop `lark.service`, back up `lark.db`, and move Mint (and any `bf_emma` companion) to `af_heart`:
   ```sh
   systemctl --user stop lark.service
   sqlite3 ~/.local/share/lark/lark.db ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   sqlite3 ~/.local/share/lark/lark.db "UPDATE companions SET voice='af_heart' WHERE voice='bf_emma';"
   ```
2. Run `./scripts/setup-host.sh --gpu-index 0`.

### Verification
- `lark status` shows `ok=true` and all terminals online.
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, voice FROM companions;"` shows `mint` using `af_heart` and no `bf_emma`.

---

## 2026-11-22 — Listening Mode & Firmware (`0.15.0`) (historical)

**Historical.** The duration column added here was replaced by the boolean
`listening` setting in the 2026-11-23 entry above; that entry migrates a
database that still has the old column, so no separate action is needed.

### Affects
- SQLite / Lark Server & MCP / Agents / Firmware (`0.15.0`)

### Breaking Changes
- **Companion `listen_seconds`**: Added `companions.listen_seconds INTEGER NOT NULL DEFAULT 0` (0–300), exposed in `state` `me.companion` and `patch.companion.listen_seconds`.
- **Listening mode**: With `listen_seconds > 0`, the voice-owner terminal opens a server-controlled listening window after playback ack or Delta Chat notice read-out, with a corner icon + bottom countdown bar, tap-to-end, filler-transcript filtering, `[silent]` no-reply support, and `replying to <Sender> (Delta Chat)` turn labelling.
- **Firmware `0.15.0`**: Added `LARK_REC_TARGET_LISTEN` (`1 << 6`) on `REC_CTRL`, skipping the start tone, local VAD watchdog, and 60 s cap (305 s safety cap) for server-controlled listening windows.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db "PRAGMA table_info(companions);"` lacks `listen_seconds`.
- Terminals report firmware `< 0.15.0`.

### Upgrade Steps
1. Stop `lark.service`, back up `lark.db`, and add `listen_seconds`:
   ```sh
   systemctl --user stop lark.service
   sqlite3 ~/.local/share/lark/lark.db ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   sqlite3 ~/.local/share/lark/lark.db "ALTER TABLE companions ADD COLUMN listen_seconds INTEGER NOT NULL DEFAULT 0;"
   ```
2. Run `./scripts/setup-host.sh --gpu-index 0`.
3. Build and OTA firmware `0.15.0` to all boards (`vocat-33d8` → `vocat-2cac` → `box3-b324`).

### Verification
- `lark status` shows `ok=true` and all terminals online on `0.15.0`.
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, listen_seconds FROM companions;"` shows `0` by default.

---

## 2026-11-22 — Pixel Overhaul, Books as Data, Weather & Firmware (`0.14.0`) (superseded)

**Superseded — no action.** The weather service, JS apps, and SVG look path
were removed by later entries. The steps below are historical and must not run
on a current installation.

### Affects
- Host / Config / SQLite / Lark Server & MCP / Agents / Firmware (`0.14.0`)

### Breaking Changes
- **Vector/SVG stack removed**: Deleted `storybook*`, `companion_art.go`, `companion_parts.go`, `screen_svg.go`, `scene.go`, `pixel.go`, `apps_ui.go`, `apps_screen.go`, `oksvg`/`rasterx`, and Go TTF fonts. All screens render on the logical pixel grid (VoCat `120×120 @3×`, BOX-3 `160×120 @2×`) with Dogica Pixel / Bold physical text.
- **Companion schema & look path**: Removed `face_svg`, `face_bg`, `mark_color`, `accent_color`, `preset`, and `face_edits`. Added `companions.form` and `companions.portrait_id`. All looks go through `companion.portrait` with `form` and generate a per-companion pixel background (`bg.png`).
- **Apps & Books as data**: `apps.kind` is `book`, `game`, or `tool`; removed `apps.svg` and the `reader` template. Books are rendered by the built-in pixel reader with per-companion reading progress in `companion:<id>` (`reading:<book>`). Custom JS apps use `render(state, player, g)` on the pixel canvas API (`clear`, `rect`, `sprite`, `text`, `image`, `button`).
- **Weather**: Added `LARK_LOCATION` in `lark.env` (default `Perth, Australia`), Open-Meteo geocoding cached in `meta`, and 15-minute current weather polling.
- **Firmware `0.14.0`**: Offline screen in `gfx.c` is a geometry-aware pixel icon + `"offline"` text without the 360-centric ring.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db "PRAGMA table_info(companions);"` still shows `face_svg`/`face_bg` or lacks `portrait_id`/`form`.
- `sqlite3 ~/.local/share/lark/lark.db "PRAGMA table_info(apps);"` still shows `svg`.
- `grep -q LARK_LOCATION ~/.config/lark/lark.env` fails.

### Upgrade Steps
1. Stop `lark.service` and back up `lark.db` and `lark.env`:
   ```sh
   systemctl --user stop lark.service
   sqlite3 ~/.local/share/lark/lark.db ".backup ~/.local/share/lark/lark.db.bak.$(date +%Y%m%d-%H%M%S)"
   cp ~/.config/lark/lark.env ~/.config/lark/lark.env.bak.$(date +%Y%m%d-%H%M%S)
   ```
2. Apply SQLite schema & data updates:
   ```sql
   ALTER TABLE companions ADD COLUMN form TEXT NOT NULL DEFAULT '';
   ALTER TABLE companions ADD COLUMN portrait_id TEXT NOT NULL DEFAULT '';
   UPDATE companions SET form='an old chameleon', portrait_id='72120f6660ef1a2151b318904197b15e' WHERE id='greg';
   UPDATE companions SET form='a cheerful orange tabby cat' WHERE id='cake' AND form='';
   UPDATE companions SET form='a fresh mint-green fox' WHERE id='mint' AND form='';
   UPDATE companions SET form='a curious little hedgehog' WHERE id='george' AND form='';
   ALTER TABLE companions DROP COLUMN face_bg;
   ALTER TABLE companions DROP COLUMN mark_color;
   ALTER TABLE companions DROP COLUMN accent_color;
   ALTER TABLE companions DROP COLUMN face_svg;
   ALTER TABLE apps DROP COLUMN svg;
   ```
   (Skip `ADD COLUMN form` if already present from pixel part 1.)
3. Ensure `LARK_LOCATION=Perth, Australia` is in `~/.config/lark/lark.env` and run `./scripts/setup-host.sh --gpu-index 0`.
4. Via live `lark_*` MCP (or server/OpenCode):
   - Generate pixel portraits and backgrounds for attached companions (`cake`, `mint`, `greg`, `george`); dormant `*-classic` companions generate lazily on attach.
   - Convert `clockwork_owl` and `lost_island_reader` into `kind='book'` apps, extracting their text.
   - Convert memory stories `companion:cake/story_lost_island` and `companion:mint/user_story` into `kind='book'` apps, then delete those memory notes.
   - Delete `book_reader`.
   - Rebuild `fireworks`, `math_game`, `monsters`, `picture_math`, and `spot_the_difference` on the pixel canvas API as `kind='game'` or `'tool'`, keeping their `app:*` state.
5. Build and OTA firmware `0.14.0` to all boards (`vocat-33d8` → `vocat-2cac` → `box3-b324`).

### Verification
- `lark status` shows `ok=true` and all terminals online on `0.14.0`.
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, form, portrait_id FROM companions;"` shows forms and portrait IDs for attached companions.
- `sqlite3 ~/.local/share/lark/lark.db "SELECT name, kind FROM apps ORDER BY name;"` shows `book` / `game` / `tool` and no `book_reader`.

---

## 2026-11-22 — Delta Chat Messaging & Hardware-Only Protocol (`0.13.0`)

### Affects
- Host / Config / Systemd / SQLite / Lark Server / Firmware (`0.13.0`)

### Breaking Changes
- **Messaging moved to Delta Chat**: Removed live calls (their call request and control messages, RTP payload types 97/98), in-house intercom, and chat/history/apps/timers/memory/portraits/tunnels HTTP routes.
- **Web UI & parental controls removed**: Removed `lark/web/`, browser `/ws`, PWA, `FRAME_SVG`, `WATCH`, `TEXT_INPUT`, `PROMPT*`, `PEERS`, `EVENT`, `kind='web'` terminals, `/api/opencode/pair`, `lark-opencode-lan.{socket,service}`, `parent_settings`, and `parent_usage`.
- **CLI admin**: Added `lark status`, `lark companions [add|rm]`, `lark terminals [attach|detach]`.
- **QR Pairing**: Pair phones by scanning the QR on the device's **Pair** menu screen; remove contacts via **Unpair**.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db ".tables"` lists `parent_settings`, `parent_usage`, or `tunnels`, or `companions` lacks `dc_account_id`.
- `systemctl --user list-units 'opentunnel*' 'lark-opencode-lan*'` shows legacy tunnel/LAN-pairing units.

### Upgrade Steps
1. Stop `lark.service`, back up `lark.db` and `lark.env`, and remove Lark's `opentunnel-*` and `lark-opencode-lan.{socket,service}` units:
   ```sh
   systemctl --user stop lark.service
   systemctl --user disable --now lark-opencode-lan.socket lark-opencode-lan.service 2>/dev/null || true
   rm -f ~/.config/systemd/user/lark-opencode-lan.socket ~/.config/systemd/user/lark-opencode-lan.service
   ```
   Inspect `systemctl --user list-units 'opentunnel*'` and stop/disable/remove any units created for Lark.
2. Apply SQLite schema changes:
   ```sql
   DROP TABLE IF EXISTS parent_settings;
   DROP TABLE IF EXISTS parent_usage;
   DROP TABLE IF EXISTS tunnels;
   DELETE FROM terminals WHERE kind='web';
   ALTER TABLE companions ADD COLUMN dc_account_id INTEGER NOT NULL DEFAULT 0;
   ```
3. Ensure `ffmpeg` is installed on `PATH`, then run `./scripts/setup-host.sh --gpu-index 0`.
4. Build and OTA firmware `0.13.0` to all boards.
5. Note for users: pair phones by scanning the QR on the device's **Pair** screen.

### Verification
- `lark status` shows `ok=true opencode=true stt=true tts=true deltachat=true`, 8 companions with `dc=` account IDs, and hardware terminals online on `0.13.0`.

---

## 2026-11-22 — Unified Binary Terminal Protocol `v1` (`0.12.0`) (historical)

**Historical.** The browser client described below was later deleted; current
firmware uses only TCP and UDP on the device port. No browser step applies.

### Affects
- Lark Server (`:8766` & `/ws`) / Web (`/app`) / Firmware (`0.12.0`)

### Breaking Changes
- **One binary protocol everywhere**: Hardware (`:8766` TCP) and browsers (`/ws` binary WebSocket frames) share the `v1` 8-byte header + typed binary catalogue (`proto_version = 1`). JSON text frames on `/ws` are removed.
- **Handshake & Resume**: `HELLO` advertises `proto_version`, `kind` (`hardware`/`web`), display formats (`rgb565`/`svg`), frame codecs, and capabilities; server replies `WELCOME` (with `resume_token`, `resume_window_ms`, `seq`, `ssrc`) or `REJECT`. `RESUME` within 30 s preserves identity and voice ownership with a fresh snapshot.
- **Voice ownership, STATUS, PROMPT, RTP-in-WS**: Explicit `VOICE_OWNER`, `STATUS` (`idle|listening|thinking|speaking|blocked|done|error`), `PROMPT`/`PROMPT_REPLY`/`PROMPT_ENDED`, and 20 ms 16 kHz RTP-in-`MEDIA` WS frames for browser mic/call audio.
- **Firmware `0.12.0` required**: `0.11.0` devices cannot complete the `v1` handshake (`REJECT`), though their HTTP OTA endpoint remains reachable.

### How to Detect
- `GET /api/terminals` shows hardware firmware `< 0.12.0` or offline after server upgrade.

### Upgrade Steps
1. Back up `~/.local/share/lark/lark` and `~/.local/share/lark/lark.db`.
2. Deploy the server first (`./scripts/setup-host.sh --gpu-index 0`).
3. Build firmware `0.12.0` (`./scripts/build.sh vocat && ./scripts/build.sh box3`) and OTA each board (`lark ota --no-build [--board box3] <ip>`).
4. Hard-refresh `/app` (or let the updated Service Worker activate) so browsers load the `v1` binary client.

### Verification
- `GET /health` reports `"ok": true`.
- `GET /api/terminals` shows all hardware terminals online on `0.12.0`.
- Opening `/app` completes `HELLO`/`WELCOME` over binary `/ws`.

---

## 2026-11-22 — Companion / Terminal Identity Split (`0.11.0`)

### Affects
- SQLite / API & MCP / Web / Firmware (`0.11.0`)

### Breaking Changes
- `companions` is keyed by slug `id` (with `voice` folded in; `saved_companions` and `voices` removed).
- `devices` and `virtual_clients` are replaced by `terminals` (`vocat-<mac4>`, `box3-<mac4>`, `web-<8hex>`, nullable `companion_id`); `device_prefs` → `terminal_prefs`.
- `sessions`, `messages`, `timers`, `build_jobs`, `parent_settings`, `parent_usage`, and `tunnels` are keyed by `companion_id`; memory scopes use `companion:<id>`.
- `/api/devices*`, `/api/virtual-clients*`, `/api/companions/reassign` → `/api/companions*` and `/api/terminals*` (`attach`/`detach`); MCP addresses `companion_id`.
- `vocat` `BOARD_ID_PREFIX` changed from `lark-` to `vocat-`.

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db ".tables"` lists `devices`, `virtual_clients`, `saved_companions`, or `device_prefs`.

### Upgrade Steps
1. Stop `lark.service` and back up `lark.db`:
   ```sh
   systemctl --user stop lark.service
   cp -a ~/.local/share/lark/lark.db ~/.local/share/lark/backups/pre-redesign-b.db
   ```
2. Run in one SQLite transaction (adjust `id_map` as needed; `virt-8458606f` → `web-8458606f` on `george`):
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
- Superseded: the browser `/ws` client and the legacy `0.9.5` two-hop OTA below are gone; current firmware uses only TCP+UDP.

### How to Detect
- `LARK_DEVICE_PORT` is unset or `0`, or the LAN firewall blocks UDP on it.

### Upgrade Steps
1. Ensure `LARK_DEVICE_PORT=8766` is allowed on the LAN firewall (TCP + UDP) and rebuild/restart `lark-bridge.service`.
2. Rebuild and flash/OTA firmware for `vocat` and `box3` (`./scripts/build.sh` / `./scripts/push-ota.sh`).

### Verification
- A board connects, renders, and plays audio; `lark status` shows it online on the current protocol.

#### Devices on 0.9.5 (OTA, historical)
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

## 2026-11-22 — EchoEar → Lark Rename, Compat-Shim Removal, and Companion Unification (`e95476d`, `f4a5912`, `206da8c`) (historical)

**Historical — no action on a Lark installation.** This entry converts a
pre-rename EchoEar install.

### Affects
- Host / Config / Systemd / SQLite / OpenCode / Firmware

### Breaking Changes

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

### How to Detect
- `~/.config/echoear` or `~/.local/share/echoear` still exists, or `echoear-*` units are enabled.

### Upgrade Steps
1. Stop the legacy services, move the config/data directories to `~/.config/lark` and `~/.local/share/lark`, rename `ECHOEAR_*` env keys, and run `./scripts/setup-host.sh`.

### Verification
- `systemctl --user status lark.service lark-opencode.service` is clean and `lark status` lists the companions.

---

## 2026-11-22 — Native SQLite WAL and Split Reader/Writer Pools

### Affects
- Host / SQLite

### Breaking Changes
- Additive: WAL mode with a writer connection and read-only readers; no data change.

### How to Detect
- `PRAGMA journal_mode` on `lark.db` returns `delete` instead of `wal`.

### Upgrade Steps
1. Restart `lark.service`; the database opens in WAL mode automatically.

### Verification
- `sqlite3 ~/.local/share/lark/lark.db "PRAGMA journal_mode;"` returns `wal`.


Replaced `tursogo` with tuned native SQLite WAL. `LARK_TURSO_*` is no longer read:
```sh
sed -i '/^LARK_TURSO_/d' ~/.config/lark/bridge.env
systemctl --user restart lark-bridge.service
sqlite3 ~/.local/share/lark/bridge.db "PRAGMA journal_mode;" # expect: wal
```

---

## 2026-11-22 — Remove Photo Uploads, Ente Photo Library, and ML Photo Indexing

### Affects
- Host / SQLite / Config

### Breaking Changes
- Removed the photo pipeline and `photos` storage; superseded by companion backgrounds (`form` + generated `bg.png`).

### How to Detect
- `sqlite3 ~/.local/share/lark/lark.db ".tables"` still lists `photos`.

### Upgrade Steps
1. Drop the retired table after backing up: `DROP TABLE IF EXISTS photos;`
2. Delete any Ente credentials from `lark.env` and restart `lark.service`.

### Verification
- `.tables` no longer lists `photos`; companion portraits and backgrounds still render.

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

---

## 2026-11-22 — Message Channel Column

### Affects
- SQLite / Lark Server

### Breaking Changes
- Added `messages.channel`; pre-column rows are backfilled by "Server Cleanup" above.

### How to Detect
- `PRAGMA table_info(messages)` lacks `channel`, or rows with `channel=''` remain.

### Upgrade Steps
1. Restart `lark.service`; new rows always carry a channel. Apply the backfill SQL from "Server Cleanup".

### Verification
- `SELECT COUNT(*) FROM messages WHERE channel='';` returns 0.

Added `channel` column to `messages` (`'in_person'`, `'deltachat'`, `'intercom'`, `'timer'`).

```sql
ALTER TABLE messages ADD COLUMN channel TEXT NOT NULL DEFAULT '';
UPDATE messages SET channel = CASE
  WHEN source = 'deltachat' THEN 'deltachat'
  WHEN source = 'intercom' THEN 'intercom'
  WHEN source = 'timer' THEN 'timer'
  ELSE 'in_person'
END WHERE channel = '';
```

---

## 2026-11-22 — Companion `form` Column and Greg Chameleon Restore

Added `form TEXT NOT NULL DEFAULT ''` to `companions` and exposed `form` in `me.companion` and portrait prompts.

### Affects
- SQLite / Lark API & MCP

### Breaking Changes
- Additive column; older rows read as an empty `form` until set.

### How to Detect
```sh
sqlite3 ~/.local/share/lark/lark.db "PRAGMA table_info(companions);" | grep -q '|form|' || echo "missing form"
sqlite3 ~/.local/share/lark/lark.db "SELECT id, form FROM companions WHERE id='greg';"
```

### Upgrade Steps
1. Stop `lark.service` and back up `~/.local/share/lark/lark.db`.
2. Add the `form` column, set `greg`'s form to `'an old chameleon'`, and relink `greg` to the existing portrait sheet at `~/.local/share/lark/portraits/72120f6660ef1a2151b318904197b15e/`:
   ```sql
   ALTER TABLE companions ADD COLUMN form TEXT NOT NULL DEFAULT '';
   UPDATE companions
   SET form = 'an old chameleon',
       face_svg = '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 360"><rect width="360" height="360" fill="#F3ECFA"/><g data-pose="idle"><image href="portrait:72120f6660ef1a2151b318904197b15e:idle" width="360" height="360"/></g><g data-pose="blink"><image href="portrait:72120f6660ef1a2151b318904197b15e:blink" width="360" height="360"/></g><g data-pose="talk"><image href="portrait:72120f6660ef1a2151b318904197b15e:talk" width="360" height="360"/></g><g data-pose="think"><image href="portrait:72120f6660ef1a2151b318904197b15e:think" width="360" height="360"/></g><g data-pose="listen"><image href="portrait:72120f6660ef1a2151b318904197b15e:idle" width="360" height="360"/></g><g data-pose="sleep"><image href="portrait:72120f6660ef1a2151b318904197b15e:blink" width="360" height="360"/></g></svg>'
   WHERE id = 'greg';
   ```
3. Start `lark.service`.

### Verification
- `sqlite3 ~/.local/share/lark/lark.db "SELECT id, form FROM companions WHERE id='greg';"` returns `greg|an old chameleon`.
- `GET /api/companions/greg` includes `"form": "an old chameleon"` and references `72120f6660ef1a2151b318904197b15e`.

