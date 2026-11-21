---
name: lark-upgrade
description: Upgrade existing Lark or legacy EchoEar installations, companions, SQLite databases, and OpenCode sessions via the bridge using changelog-style upgrade notes, or record new breaking changes.
---

# Lark Upgrade

## Policy

Lark carries **zero runtime migrations and zero backwards-compatibility shims** in the codebase.

- Breaking changes append a dated entry to `references/changelog.md`.
- OpenCode agents execute upgrades directly on the host and through the live bridge using those changelog entries.
- Never reintroduce runtime schema migrations, alias routes, or legacy fallbacks into Go, JS, or firmware.

## Upgrade an Installation

1. **Identify starting state and back up before mutating anything**
   - Inspect running services (`systemctl --user status lark-bridge lark-opencode` or legacy `echoear-*`), config (`~/.config/lark/bridge.env` or `~/.config/echoear/bridge.env`), and DB (`~/.local/share/lark/bridge.db` or `~/.local/share/echoear/bridge.db`).
   - Stop the bridge before copying or editing SQLite files (`systemctl --user stop lark-bridge.service` / `echoear-bridge.service`).
   - Back up the database (for example with `sqlite3 <db> ".backup '<backup-path>'"`) and copy `bridge.env` and `opencode.jsonc` before making changes.

2. **Read `references/changelog.md` chronologically**
   - Read every entry from the installation's current commit/state up to the latest entry.
   - Apply entries in chronological order; do not skip intermediate steps.

3. **Apply offline host, config, systemd, and SQLite steps**
   - Apply any required filesystem renames, `bridge.env` key updates, systemd unit updates, and SQLite schema/data SQL from the applicable changelog entries.
   - Run `./scripts/setup-host.sh` (or `--models-only` if only assets changed) and ensure `lark-opencode.service` and `lark-bridge.service` are running cleanly.

4. **Upgrade companions, memory, apps, and OpenCode sessions via the live bridge**
   - Authenticate HTTP requests with `X-Lark-Token: <LARK_BRIDGE_TOKEN>` (from `bridge.env`) or use the `lark_*` MCP tools (`lark_state`, `lark_patch`, `lark_app`, `lark_emit`) when connected to the bridge MCP server.
   - Inspect live state:
     - `GET /health`
     - `GET /api/devices`
     - `GET /api/companions`
     - `GET /api/diagnostics`
     - `GET /api/memory/scopes` (and `GET /api/memory?scope=<scope>`)
     - `GET /api/apps`
   - Upgrade companion looks, presets, or portraits via `PUT /api/devices/{id}/companion` (or `lark_patch` with `companion.preset` / `companion.portrait`). Preserve user-chosen `name`, `personality`, `voice`, and custom `face_svg` unless a changelog entry explicitly replaces them.
   - Upgrade old or stale OpenCode sessions:
     - Inspect recent history via `GET /api/devices/{id}/messages`.
     - If important continuity notes exist, save them to `memory` (via `lark_patch` `memory`) before resetting.
     - Reset the session via `POST /api/devices/{id}/reset` (pass `{"archive_history": false}` to keep visible chat history on the device/dashboard while binding a fresh session, or `true` to archive) so the device binds cleanly to the current `lark` agent and MCP contract.
     - When only context compaction or MCP reconnection is needed, use `POST /api/opencode/compact` (optionally with `{"device_id": "<id>", "reconnect_mcp": true}`).

5. **Verify**
   - Confirm `GET /health` reports `"ok": true`, `"opencode": true`, `"stt": true`, `"tts": true`.
   - Confirm `GET /api/devices`, `GET /api/companions`, and `GET /api/diagnostics` succeed and show expected devices, companions, and voices.
   - Note any manual steps the user must complete (such as USB firmware reflash or browser `localStorage` re-pair).

## Record a New Breaking Change

When landing a breaking change in the repo, append a new dated section to `references/changelog.md` using this exact template:

```markdown
## YYYY-MM-DD — <Short Title> (<commit-or-range>)

### Affects
- Host / Config / Systemd / SQLite / Bridge API & MCP / Web / Firmware (list what applies)

### Breaking Changes
- Bullet list of removed shims, renamed tables/columns/routes/env keys, or changed contracts.

### How to Detect
- Concrete commands, SQL queries, or HTTP checks that identify an un-upgraded installation.

### Upgrade Steps
1. Exact offline host/config/SQLite commands (after backing up).
2. Exact live-bridge HTTP (`X-Lark-Token`) or `lark_*` MCP steps.

### Verification
- Exact checks (`GET /health`, `GET /api/devices`, `sqlite3` queries, firmware/browser checks) confirming the upgrade succeeded.
```
