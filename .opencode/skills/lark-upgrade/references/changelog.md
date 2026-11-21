# Lark Upgrade Changelog

Apply entries chronologically from the installation's starting state to latest. Always back up `bridge.db` and `bridge.env` before mutating anything.

---

## 2026-11-22 — EchoEar → Lark Rename, Compat-Shim Removal, and Companion Unification (`e95476d`, `f4a5912`, `206da8c`)

### Affects
- Host paths, `bridge.env`, systemd user units, and dedicated OpenCode config
- SQLite schema (`bridge.db`)
- Bridge HTTP API & MCP tools
- Browser `/app` PWA (`localStorage`)
- ESP32 firmware (`vocat` and `box3`)

### Breaking Changes
- **Compat shims removed (`e95476d`)**: Removed `bridge/migrate.go` and all runtime SQLite migrations, legacy route aliases, and legacy payload fallbacks.
- **EchoEar → Lark rename (`f4a5912`)**:
  - Config/data dirs moved from `~/.config/echoear` and `~/.local/share/echoear` to `~/.config/lark` and `~/.local/share/lark`.
  - Env vars in `bridge.env` renamed from `ECHOEAR_*` to `LARK_*` (e.g., `LARK_BRIDGE_TOKEN`, `LARK_OPENCODE_URL`, `LARK_OPENCODE_DIR`, `LARK_REPO_DIR`, `LARK_TTS_VOICE`, `LARK_OPENTUNNEL_BIN`).
  - Systemd user units renamed from `echoear-bridge.service`, `echoear-opencode.service`, `echoear-opencode-lan.socket`, `echoear-opencode-lan.service` to `lark-bridge.service`, `lark-opencode.service`, `lark-opencode-lan.socket`, `lark-opencode-lan.service`.
  - OpenCode agent renamed from `echoear` (`agents/echoear.md`) to `lark` (`agents/lark.md`), builder to `lark-builder`, and fallback plugin to `lark-fallback.js`.
  - Firmware project names changed to `lark` (`vocat`) and `lark_box3` (`box3`); Kconfig options renamed to `CONFIG_LARK_*`; device HTTP header changed to `X-Lark-Token`; telemetry fields use `wifi_rssi` and `call_pcm`.
- **Companion domain unification (`206da8c`)**:
  - SQLite tables `cats` → `companions` and `saved_cats` → `saved_companions`; dropped legacy columns (`extras_json`, `svg_extras`, `theme`, `chat_events`, `schema_version`, `history_backfill`).
  - `messages.role` values `'cat'` → `'companion'`.
  - HTTP routes `/api/cats`, `/api/cats/reassign`, `/api/devices/{id}/cat` → `/api/companions`, `/api/companions/reassign`, `/api/devices/{id}/companion`.
  - MCP `patch` field `cat` → `companion`; `state` field `me.cat` → `me.companion`; `lark_*` MCP tool prefix.
  - Web client `localStorage` keys updated (`lark-token`, `lark-phone`).

### How to Detect
- Legacy directories/units exist: `~/.config/echoear`, `~/.local/share/echoear`, or `systemctl --user list-unit-files 'echoear*'`.
- Legacy SQLite tables or columns exist:
  ```sh
  sqlite3 ~/.local/share/lark/bridge.db ".tables"
  # Un-upgraded if `cats` or `saved_cats` appear, or `SELECT count(*) FROM messages WHERE role='cat'` > 0.
  ```
- Stale OpenCode sessions or `lark` agent mismatch visible in `GET /api/diagnostics` or failing turns after upgrade.

### Upgrade Steps

#### 1. Stop services and back up
```sh
systemctl --user stop echoear-bridge.service echoear-opencode.service echoear-opencode-lan.socket 2>/dev/null || true
systemctl --user stop lark-bridge.service lark-opencode.service lark-opencode-lan.socket 2>/dev/null || true

# Back up whichever directory exists
mkdir -p ~/.local/share/lark/backups
if [ -f ~/.local/share/echoear/bridge.db ]; then
  sqlite3 ~/.local/share/echoear/bridge.db ".backup '$HOME/.local/share/lark/backups/pre-upgrade.db'"
elif [ -f ~/.local/share/lark/bridge.db ]; then
  sqlite3 ~/.local/share/lark/bridge.db ".backup '$HOME/.local/share/lark/backups/pre-upgrade.db'"
fi
```

#### 2. Migrate host directories, `bridge.env`, and systemd units
```sh
mkdir -p ~/.config/lark ~/.local/share/lark
[ -d ~/.config/echoear ] && cp -a ~/.config/echoear/. ~/.config/lark/
[ -d ~/.local/share/echoear ] && cp -a ~/.local/share/echoear/. ~/.local/share/lark/

# Rename ECHOEAR_* keys to LARK_* in ~/.config/lark/bridge.env
if [ -f ~/.config/lark/bridge.env ]; then
  sed -i 's/^ECHOEAR_/LARK_/' ~/.config/lark/bridge.env
fi

# Disable legacy echoear user units if present
systemctl --user disable --now echoear-bridge.service echoear-opencode.service echoear-opencode-lan.socket echoear-opencode-lan.service 2>/dev/null || true
```

#### 3. Migrate SQLite schema (`bridge.db`) to match `bridge/db.go`
Run on `~/.local/share/lark/bridge.db` while `lark-bridge.service` is stopped. Copy-and-verify into clean tables, then drop legacy tables/columns:

```sql
PRAGMA foreign_keys = OFF;
BEGIN IMMEDIATE;

-- Ensure target tables exist with exact current definitions from bridge/db.go
CREATE TABLE IF NOT EXISTS companions (
  device_id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  configured INTEGER NOT NULL DEFAULT 0,
  face_bg TEXT NOT NULL,
  mark_color TEXT NOT NULL,
  accent_color TEXT NOT NULL,
  personality TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  face_svg TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS saved_companions (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  face_bg TEXT NOT NULL,
  mark_color TEXT NOT NULL,
  accent_color TEXT NOT NULL,
  personality TEXT NOT NULL DEFAULT '',
  voice TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  face_svg TEXT NOT NULL DEFAULT ''
);

-- Copy from legacy `cats` if present (ignore if already migrated)
INSERT OR IGNORE INTO companions (device_id, name, configured, face_bg, mark_color, accent_color, personality, updated_at, face_svg)
SELECT device_id, name, COALESCE(configured, 0), face_bg, mark_color, accent_color, COALESCE(personality, ''), updated_at, COALESCE(face_svg, '')
FROM cats;

-- Copy from legacy `saved_cats` if present
INSERT OR IGNORE INTO saved_companions (slug, name, face_bg, mark_color, accent_color, personality, voice, updated_at, face_svg)
SELECT slug, name, face_bg, mark_color, accent_color, COALESCE(personality, ''), COALESCE(voice, ''), updated_at, COALESCE(face_svg, '')
FROM saved_cats;

-- Update message roles
UPDATE messages SET role = 'companion' WHERE role = 'cat';

-- Clear stale OpenCode session bindings and legacy meta keys
DELETE FROM sessions;
DELETE FROM meta WHERE key IN ('schema_version', 'history_backfill');

-- Drop legacy tables
DROP TABLE IF EXISTS cats;
DROP TABLE IF EXISTS saved_cats;
DROP TABLE IF EXISTS chat_events;

COMMIT;
PRAGMA integrity_check;
```

*(If `cats`, `saved_cats`, or `chat_events` do not exist in a partially upgraded DB, run only the statements for tables that exist.)*

#### 4. Reinstall host services and dedicated OpenCode config
```sh
./scripts/setup-host.sh
```
Confirm `lark-opencode.service`, `lark-opencode-lan.socket`, and `lark-bridge.service` are active.

#### 5. Upgrade companions and OpenCode sessions via the live bridge
Using `TOKEN=$(grep '^LARK_BRIDGE_TOKEN=' ~/.config/lark/bridge.env | cut -d= -f2-)` and `BASE=http://127.0.0.1:8765`:

1. Inspect current state:
   ```sh
   curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/health"
   curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/api/devices"
   curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/api/companions"
   curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/api/memory/scopes"
   curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/api/apps"
   ```
2. For each device (`{id}`):
   - Inspect recent history via `GET /api/devices/{id}/messages`; if any important continuity notes should survive a fresh OpenCode session, save them into `memory` with `lark_patch`.
   - Reset the session binding so the next turn binds to the `lark` agent and current `lark_*` MCP contract without erasing visible history:
     ```sh
     curl -fsS -X POST -H "X-Lark-Token: $TOKEN" -H "Content-Type: application/json" \
       -d '{"archive_history":false}' "$BASE/api/devices/{id}/reset"
     ```
   - Upgrade companion look/preset/portrait if needed via `PUT /api/devices/{id}/companion` (or `lark_patch` with `companion.preset` / `companion.portrait`), preserving user-chosen `name`, `personality`, `voice`, and custom `face_svg`.

#### 6. Browser and Firmware
- **Browser `/app`**: Re-enter the bridge token in `/app` (`localStorage` key `lark-token`) and re-join any browser virtual companion (`lark-phone`).
- **Firmware (`vocat` and `box3`)**: Update `firmware/sdkconfig.local` (`CONFIG_LARK_*` keys from `firmware/sdkconfig.local.example`) and **USB-reflash once** (`./scripts/flash-usb.sh /dev/ttyACM0`, or `BOARD=box3 ./scripts/flash-usb.sh /dev/ttyACM0`). OTA cannot cross the `echoear` → `lark` / `lark_box3` project name boundary because `ota_server.c` enforces `esp_app_get_description()->project_name`.

### Verification
- `curl -fsS -H "X-Lark-Token: $TOKEN" "$BASE/health"` returns `{"ok":true,"opencode":true,"stt":true,"tts":true,...}`.
- `GET /api/devices`, `GET /api/companions`, and `GET /api/diagnostics` return HTTP 200 with expected devices and companions.
- `sqlite3 ~/.local/share/lark/bridge.db "SELECT name FROM sqlite_master WHERE type='table'"` contains `companions` and `saved_companions` and no `cats`, `saved_cats`, or `chat_events`.
- Send a test chat message to a device and confirm the `lark` agent responds and `lark_state` / `lark_patch` work cleanly.

---

## 2026-11-22 — Native SQLite WAL, Split Reader/Writer Pools, and Disk-Backed Photo Blobs

### Affects
- `bridge.env` (`LARK_TURSO_*` removed)
- SQLite schema (`photos` table in `lark.db`)
- Photo blob storage (`~/.local/share/lark/blobs/photos/<id>`)

### Breaking Changes
- **Removed `tursogo` and Turso Cloud sync**: Replaced `turso.tech/database/tursogo` with tuned native SQLite (`github.com/mattn/go-sqlite3`) in WAL mode (`_synchronous=NORMAL`, `mmap_size=256MB`, `cache_size=-32MB`, `temp_store=MEMORY`) and split reader/writer pools. `LARK_TURSO_URL` and `LARK_TURSO_AUTH_TOKEN` are no longer read.
- **Disk-backed photo blobs**: The `photos` table replaced `data BLOB` with `path TEXT NOT NULL` pointing to `~/.local/share/lark/blobs/photos/<id>` (relative path `blobs/photos/<id>`).

### How to Detect
- `photos` table still has a `data` column instead of `path`:
  ```sh
  sqlite3 ~/.local/share/lark/lark.db "PRAGMA table_info(photos);"
  ```
- `LARK_TURSO_URL` or `LARK_TURSO_AUTH_TOKEN` still present in `~/.config/lark/bridge.env`.

### Upgrade Steps

#### 1. Stop `lark-bridge.service` and back up
```sh
systemctl --user stop lark-bridge.service
mkdir -p ~/.local/share/lark/backups ~/.local/share/lark/blobs/photos
sqlite3 ~/.local/share/lark/lark.db ".backup '$HOME/.local/share/lark/backups/pre-sqlite-upgrade.db'"
```

#### 2. Extract legacy `photos.data` BLOBs to disk and migrate `photos` table
```sh
python3 - <<'PY'
import os, sqlite3

data_dir = os.path.expanduser("~/.local/share/lark")
db_path = os.path.join(data_dir, "lark.db")
blob_dir = os.path.join(data_dir, "blobs", "photos")
os.makedirs(blob_dir, mode=0o700, exist_ok=True)

conn = sqlite3.connect(db_path)
cols = [row[1] for row in conn.execute("PRAGMA table_info(photos)")]
if "data" in cols:
    conn.execute("BEGIN IMMEDIATE")
    conn.execute("""
        CREATE TABLE photos_new (
            id TEXT PRIMARY KEY,
            device_id TEXT NOT NULL,
            mime TEXT NOT NULL,
            path TEXT NOT NULL,
            caption TEXT NOT NULL DEFAULT '',
            created_at INTEGER NOT NULL
        )
    """)
    for pid, device_id, mime, data, caption, created_at in conn.execute(
        "SELECT id, device_id, mime, data, caption, created_at FROM photos"
    ):
        rel_path = os.path.join("blobs", "photos", pid)
        abs_path = os.path.join(data_dir, rel_path)
        with open(abs_path, "wb") as f:
            f.write(bytes(data))
        os.chmod(abs_path, 0o600)
        conn.execute(
            "INSERT INTO photos_new (id, device_id, mime, path, caption, created_at) VALUES (?,?,?,?,?,?)",
            (pid, device_id, mime, rel_path, caption, created_at),
        )
    conn.execute("DROP TABLE photos")
    conn.execute("ALTER TABLE photos_new RENAME TO photos")
    conn.commit()
conn.close()
PY
```

#### 3. Clean up `bridge.env` and restart
Remove any `LARK_TURSO_URL` / `LARK_TURSO_AUTH_TOKEN` lines from `~/.config/lark/bridge.env`, rebuild/install the bridge, and start `lark-bridge.service`:
```sh
sed -i '/^LARK_TURSO_/d' ~/.config/lark/bridge.env
systemctl --user start lark-bridge.service
```

### Verification
- `sqlite3 ~/.local/share/lark/lark.db "PRAGMA journal_mode; PRAGMA table_info(photos);"` reports `wal` and a `path` column (no `data` column).
- `GET /api/photos/{id}` serves existing migrated photos with HTTP 200 and `ETag`.
