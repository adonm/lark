# Lark Upgrade Changelog

Apply entries chronologically from the installation's starting state to latest. Always back up `lark.db` and `lark.env` (or `bridge.env`) before mutating anything.

---

## 2026-11-23 — Silent Story Opens

### Affects
- Lark Server / Agents

### Changes
- Opening a story from the device menu, Memories → Reading, or the panel's Open/Start over shows the passage silently — no TTS and nothing queued. Voice choices still narrate the next passage, and conversational story reads are unchanged.
- `openStory`/`advanceStoryPassage` take an explicit narration flag; menu and panel entry points pass `false`.

### Upgrade Steps
1. Restart `lark.service`.

---

## 2026-11-23 — OpenCode Zen Gateway

### Affects
- Config / Lark Server / Agents

### Changes
- **Models on `opencode` Zen**: `lark` now uses `opencode/deepseek-v4.1-flash#low`, the `deep` subagent `opencode/glm-5.3#high`, and the router fallback `opencode/glm-5.3-flash#low` (previously all `opencode-go/…`). `setup-opencode-config.py` rewrites only an untouched generated config; custom model choices are kept. The fallback plugin still requires the fallback on the same provider.
- **Auth**: the dedicated instance imports `opencode` credentials once via `setup-host.sh` and marks `state/imported-provider-opencode`; no manual step when that marker exists.
- **Measured** (20 interleaved warm chat turns each, same host): `session.step.started` p50/p95 **1,087/1,448 ms** on `opencode-go` vs **1,058/1,523 ms** on `opencode`; first text delta **1,087/1,448** vs **1,066/1,523 ms**; uncached input 18–139 tokens, cache read 3,456–3,712; 5/5 hard-prompt rubric (timer, story, intercom, reasoning, deep delegation).

### Upgrade Steps
1. Re-run `./scripts/setup-host.sh` (migrates the generated config and imports Zen auth if missing), or edit `opencode.jsonc` to the three ids above and restart `lark-opencode.service`.

## 2026-11-23 — Webxdc Panel v4: Companion Name, History Echo Fix

### Affects
- Lark Server / Webxdc (`dc_panel` v4)

### Changes
- **Panel v4 naming**: The webxdc manifest `name` is the companion's display name and `icon.png` is the companion's rasterised portrait. Renaming a companion re-sends the panel (`dc_panel` meta stores `<version>:<name>`) and deletes the previous message. The header logo and "Lark Control" branding are gone; `/help` says "/panel — open my panel".
- **History/voice fixes**: Status updates pushed by the bot are no longer re-applied as commands (a history response could spawn another history request recursively; one live panel reached >700k updates). The panel correlates history responses by `sid`/`seq`, ignores replays, and applies the voice from the `set_voice` `ack`.

### Upgrade Steps
1. Restart `lark.service`. On the next startup sync (or `/panel`) each verified contact gets the v4 panel named after the companion, and the old panel message is deleted for everyone (`dcPanelVersion` `"4.1"`; the patch bump re-sends the panel with the own-echo fix). No DB migration is needed; stale `msgs_status_updates` rows disappear with the old message.

---

## 2026-11-22 — Webxdc Panel Upgrade, Old Panel Cleanup & History Tab

### Affects
- Lark Server / Webxdc (`dc_panel` v3)

### Changes
- **Old panel & story cleanup**: Track `dc_panel_msg:` and `dc_story_msgs:` in `meta`; on panel version bump, `/panel` resend, or `/story <name>` resend, delete previous messages for everyone via `DeleteMessagesForAll` (including a one-off scan of existing `"Lark Control"` webxdc messages on first v3 upgrade).
- **Panel v3**: Added live companion state header pushes (throttled, active-panel only), quick Say input, story progress bars with Continue/Start over, curated voice preview taps, and a History tab with channel filters, `LIKE` search, and `(ts_ms, id)` cursor pagination.

### Upgrade Steps
1. Restart `lark.service`; existing verified contacts automatically receive the v3 panel and have older panel messages deleted on next startup sync or `/panel`.

---

## 2026-11-22 — Semantic End-of-Turn (Smart Turn v3.2) & Malloc Arena Cap

### Affects
- Host (`stt-models`, `lark.service`) / Lark Server

### Changes
- **Smart Turn v3.2**: Added in-process Whisper log-mel + `smart-turn-v3.2-cpu.onnx` (via `onnxruntime_go` and bundled `libonnxruntime.so`) for ~200 ms pause semantic end-of-turn detection, falling back to tuned hangover.
- **RSS plateau**: Added `MALLOC_ARENA_MAX=2` to `lark.service` and `mallopt(M_ARENA_MAX, 2)` on Linux to bound native C/C++ arena fragmentation.

### Upgrade Steps
1. Re-run `./scripts/setup-host.sh` (or `--models-only` + rebuild) to download `smart-turn-v3.2-cpu.onnx` into `~/.local/share/lark/stt-models/` and restart `lark.service`.

---

## 2026-11-22 — Interactive Stories (Twee 3), Feature Cuts & Firmware (`0.18.0`)

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
- **Webxdc control panel**: Added `dcPanelVersion` (`"1"`), tracked in `meta` (`dc_panel:<companion_id>:<contact_addr>` and `dc_panel_serial:<acc_id>:<msg_id>`). Sent via `/panel` and automatically on startup/SecureJoin when the panel version changes.
- **Startup & SecureJoin delivery**: On startup (`syncWelcomes`) and on SecureJoin verification (`EventTypeSecurejoinInviterProgress`), companions send the welcome message and control panel once per version to verified human contacts (never to companions).

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

## 2026-11-22 — Listening Mode & Firmware (`0.15.0`)

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

## 2026-11-22 — Pixel Overhaul, Books as Data, Weather & Firmware (`0.14.0`)

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
- **Messaging moved to Delta Chat**: Removed live calls (`CALL_REQ`, `CALL_CTRL`, RTP PT 97/98), in-house intercom, and chat/history/apps/timers/memory/portraits/tunnels HTTP routes.
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

## 2026-11-22 — Unified Binary Terminal Protocol `v1` (`0.12.0`)

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

---

## 2026-11-22 — Message Channel Column

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

