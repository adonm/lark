---
name: lark-upgrade
description: Upgrade existing Lark or legacy EchoEar installations, companions, SQLite databases, and OpenCode sessions via the Lark server using changelog-style upgrade notes, or record new breaking changes.
---

# Lark Upgrade

## Policy

Lark carries **zero runtime migrations and zero backwards-compatibility shims**
in the codebase. The only exception is the guarded, idempotent column rename
and drop in `initSchema`, which is listed in the changelog like any other
one-time step.

- Breaking changes append a dated entry to `references/changelog.md`.
- OpenCode agents execute upgrades directly on the host and through the live Lark server using those changelog entries.
- Never reintroduce runtime schema migrations, alias routes, or legacy fallbacks into Go, JS, or firmware.
- Mark an entry whose features were later deleted with a bold
  `**Superseded — no action.**` lead. Retired names may appear inside such an
  entry; keep them out of live instructions.

## Upgrade an Installation

1. **Identify starting state and back up before mutating anything**
   - Inspect running services (`systemctl --user status lark lark-opencode`), config (`~/.config/lark/lark.env`), and DB (`~/.local/share/lark/lark.db`).
   - Stop the Lark server before copying or editing SQLite files (`systemctl --user stop lark.service`).
   - Back up the database (`sqlite3 <db> ".backup '<backup-path>'"`) and copy `lark.env` and the OpenCode `opencode.jsonc`.
   - Legacy EchoEar installs still use `~/.config/echoear`, `~/.local/share/echoear`, `echoear-*` units, and tables such as `cats`; convert them while the service is stopped.

2. **Read `references/changelog.md` newest first, apply bottom-up**
   - Entries are ordered newest first; apply them from the oldest applicable entry up to the newest.
   - Do not skip intermediate entries unless the entry leads with
     `Superseded — no action.`

3. **Apply offline host, config, systemd, and SQLite steps**
   - Apply the changelog's filesystem renames, `lark.env` key updates, systemd unit updates, and SQLite schema/data SQL.
   - Run `./scripts/setup-host.sh --gpu-index 0`, then confirm `lark.service` and `lark-opencode.service` are running cleanly. For a server-only update, `~/.local/share/lark/lark deploy` also refreshes the dedicated OpenCode config.

4. **Upgrade companions, memory, and stories through the live server**
   - Authenticate HTTP requests with `X-Lark-Token: <LARK_TOKEN>` (from `lark.env`), or use the `lark_*` MCP tools: `state`, `patch`, `story`, `library`, `emit`.
   - Inspect live state:
     - `GET /health`
     - `GET /api/companions`
     - `GET /api/companions/<id>`
     - `GET /api/terminals`
     - `GET /api/diagnostics`
     - `GET /api/library`
     - `GET /api/remote/tunnels`
   - Companion changes use the `patch` tool (or `PATCH /api/companions/<id>` with the same body): `companion.name`, `companion.form` (regenerates the background), `companion.voice`, `companion.portrait`, `companion.listening`, and `memory` entries. Preserve user-chosen names, personalities, voices, and portraits unless a changelog entry explicitly replaces them.
   - Reset a stale or broken OpenCode session with `POST /api/companions/<id>/reset` (`{"archive_history": true}` keeps visible history in an archive; omit or `false` to bind a fresh session and drop the old one).
   - Inspect recent history through the `state` tool (`include: ["messages"]`) or the `/remote` page; save important continuity notes to memory before resetting.
   - Delete companions with `DELETE /api/companions/<id>`; detach or remove their terminals first (`DELETE /api/terminals/<id>`). Deletion removes history, timers, story progress, memory scope, Delta Chat welcome state, remote links, and the portrait directory.

5. **Verify**
   - Confirm `GET /health` reports `"ok": true`, `"opencode": true`, `"stt": true`, `"tts": true`.
   - Confirm `GET /api/companions`, `GET /api/terminals`, and `GET /api/diagnostics` succeed and show the expected companions, terminals, voices, and no retired telemetry keys.
   - After an OpenCode upgrade, confirm the experimental instruction-entry route still matches the changelog: `PUT /api/experimental/session/<sid>/instructions/entries/lark.companion` with `{"value":{"id":"…","name":"…","personality":"…"}}` returns 204, and `GET /api/experimental/session/<sid>/instructions/entries` lists it. Lark writes this entry at session creation and refreshes it when a name or personality edit lands; a missing or changed route breaks session creation loudly.
   - Note any manual step the user must complete (such as USB firmware reflash). There is no browser pairing step and no `localStorage` state.

## Record a New Breaking Change

Append a dated section at the **top** of `references/changelog.md` (newest first) using this exact structure:

```markdown
## YYYY-MM-DD — <Short Title> (<commit-or-range>)

### Affects
- Host / Config / Systemd / SQLite / Lark API & MCP / Web / Firmware (list what applies)

### Breaking Changes
- Bullet list of removed shims, renamed tables/columns/routes/env keys, or changed contracts.

### How to Detect
- Concrete commands, SQL queries, or HTTP checks that identify an un-upgraded installation.

### Upgrade Steps
1. Exact offline host/config/SQLite commands (after backing up).
2. Exact live-server HTTP (`X-Lark-Token`) or `lark_*` MCP steps.

### Verification
- Exact checks (`GET /health`, `GET /api/companions`, `sqlite3` queries, firmware checks) confirming the upgrade succeeded.
```
