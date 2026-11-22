# Lark

[Lark](https://github.com/adonm/lark) is a tap-to-talk companion with interactive diagrams, voice notes, and live calls. It starts as a friendly cat, then can shift into any creature or character a child imagines.

```text
Device  <-- TCP/UDP :8766: binary frames, touch, RTP PCM --> Lark server :8765 (HTTP admin, MCP, OTA)
                                              |-- Parakeet (in-process Vulkan speech to text)
                                              |-- OpenCode :49375 (loopback, reply deltas over SSE)
                                              `-- Kokoro   (FP32, embedded via sherpa-onnx)
```

**Targets:** ESP-VoCat v1.2 and ESP32-S3-BOX-3. Both use a 360×360 logical canvas,
centered on the BOX-3’s rectangular display. Lark runs apps and renders
RGB565 updates for hardware. Boards draw a fallback face offline.

**New in 0.9.3:** Both boards use the touchscreen for all touch controls.
VoCat’s capacitive head pads are disabled.

**New in 0.9.2:** Live two-way **Call** and a paged voice picker in the
on-device picker. Firmware announces features in `hello` and flushes only the
dirty LCD columns within changed rows. The server and dashboard add paginated
chat history, faster face rendering, and separate turn and hardware diagnostics.

## Quickstart

1. On Linux, install Go 1.26, GCC, a C++17 compiler, CMake ≥3.19, Ninja,
   Vulkan drivers and development headers/loader, `glslc`, the SPIR-V Headers
   CMake package, Python 3.11+, OpenCode V2, Podman, `uvx` (USB flashing only),
   and `curl`. Use a systemd user session.

   ```sh
   ./scripts/setup-host.sh
   ```

   Setup builds the `lark` binary with in-process Vulkan STT, installs the
    SHA-256-pinned Parakeet Q8_0 GGUF, Silero v5, and FP32 `kokoro-multi-lang-v1_0`
    pack, and starts the Lark server and a dedicated OpenCode service. On a multi-GPU
   host, use `vulkaninfo --summary` and pass `--gpu-index INDEX` for the
   intended device. Lark runs Kokoro in-process via
   `sherpa-onnx-go` and checksum-verified libraries in `$ORIGIN/lib`.
   The Vulkan loader and driver come from the OS. Host setup still uses
   Python; the server has no Python runtime or TTS subprocess. Set
   `TTS_MODEL_DIR` in `lark.env` to load that pack from a different directory.
   Set `LARK_REPO_DIR` when launching outside the checkout to locate the
   agent prompt and OTA files (the systemd unit sets it). Choose the models in
    the [dedicated OpenCode config](#companion-models).

    STT needs a Vulkan GPU, not Hex or an X display. Lark recognizes
    English without custom vocabulary. On firmware 0.9.5+,
    Lark listens through thinking pauses and uses local transcription
    to decide when to send a turn. Clients without the capability keep their
    local VAD. You can still Send or Cancel while the mic is open. See
    [endpoint policy and replay results](lark/turn-taking.md) and
    [STT measurements and artifact pins](lark/stt-speed.md).

2. Create the private firmware config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set `CONFIG_LARK_WIFI_SSID`, `CONFIG_LARK_WIFI_PASSWORD`,
   `CONFIG_LARK_URL` (`http://<lark-host>:8765` or `lark://<lark-host>:8766`, no trailing slash), and
   `CONFIG_LARK_TOKEN` (copy `LARK_TOKEN` from
   `~/.config/lark/lark.env`, or use `$XDG_CONFIG_HOME`).
   Both board builds use the ESP-IDF 5.5 container and read the same
   `firmware/sdkconfig.local`. Never commit that file or share the resulting binaries.

3. Build and flash the ESP-VoCat v1.2 (`vocat`, 360×360 round):

   ```sh
   ./scripts/build.sh
   ./scripts/flash-usb.sh /dev/ttyACM0
   ./scripts/monitor.sh /dev/ttyACM0
   ```

   For the Espressif ESP32-S3-BOX-3 (320×240 rectangular, USB power):

   ```sh
   BOARD=box3 ./scripts/build.sh
   BOARD=box3 ./scripts/flash-usb.sh /dev/ttyACM0
   ```

   BOX-3 builds into `firmware/build-box3/` instead of `firmware/build/`.
   It starts with a blank cat at `box3-xxxx.local` (last four MAC digits)
   and has no battery gauge.

4. Update via `~/.local/share/lark/lark ota <ip|host>`; add `--no-build` to reuse the
   image, and prefix with `BOARD=box3` for BOX-3. The script refuses a board
   mismatch. Direct OTA works without the server and reads the token from the
    environment or `lark.env`. The board confirms the new image after display
    initialization and Wi-Fi DHCP; the script checks the running partition.
   Builds use `lark` and `lark_box3` as the firmware project names.

5. Run `./scripts/check.sh` for ShellCheck (if installed), Node tests, Go vet
   and race tests, strict GCC/ASan/UBSan host tests for both board layouts,
   and tracked-file hygiene. GitHub Actions runs this offline without model
   weights or a Vulkan GPU; it does not build ESP-IDF images. Build those locally with the
   commands above (`BOARD=vocat` and `BOARD=box3`).

   Add `--live` to exercise the product with virtual companions and firmware
   clients for both board layouts. It builds a fresh loopback server with a
   temporary database, real Parakeet Vulkan, Silero and Kokoro, synthetic
   photos, and a deterministic model stub. It does not connect to the installed server, use
   model quota, or test wake word. Allow at least two minutes for the actual
   idle timeout. Set `LARK_PARAKEET_NATIVE` to the `native/` prefix from
   `scripts/build-parakeet.sh` to reuse its verified static libraries.
   `scripts/setup-host.sh --models-only` installs the pinned Kokoro and Silero
   assets without replacing the server or restarting services.

   `--e2e-real` uses the dedicated local OpenCode server, its configured companion
   model and fallback, and disposable sessions; it can spend model quota.
   `--deployed` runs the older deployed-service and OTA probes (no upload or
   flash).
   `--qa` runs the repeatable `lark sim` QA suite (`lark/qa/*.sim`); `--qa-hw`
   adds the live hardware QA suite. `--panel-ui` runs the headless-Chromium
   webxdc panel tests (needs `playwright` or `playwright-core`; set
   `LARK_PLAYWRIGHT_MODULE` if it is not in a standard location).

## Controls, voice notes, and calls

### Using a companion

- Tap the companion's face and speak after the tone. Pause to send
  automatically, or tap **Send** to finish early, **Cancel** to discard, or
  **Stop** to interrupt a reply.
- The display dims after 120 seconds idle. Tap the touchscreen to wake it.
- The on-device **Menu** provides touch controls:
  **Games** launches saved apps, **Memories** shows the companion's saved memories,
  and **History** shows the last 20 messages, newest first. Tap a row for details.
  **Talk to…** opens intercom, **Volume** sets the level or enables **Auto**,
  **Voice** pages through six curated voices, and **Stats** opens the diagnostics HUD.
  Auto-volume estimates ambient noise from recordings and adjusts the DAC in RAM
  without writing to flash. **Home** returns to the companion.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” Lark bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or choose a peer in
  **Menu → Talk to…** to record directly to that companion.
- Tap **Call** on an incoming Intercom card, pick a peer in **Menu → Call**, or
  say “call \<name\>.” Both devices must be online and call-capable. The recipient
  can answer or decline; either side can mute its mic or hang up. Unlike a
  voice note, a call streams both ways in real time.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  `emit` to deliver a card and synthesized speech to the recipient.

### Voices and diagnostics

Companions choose from six warm, kid-friendly storyteller voices:
Heart, Bella, and Puck (US English), or Fable, Emma, and George (UK English).
The choice is saved per companion in the Lark database. Mint defaults to Fable
(`bm_fable`). Cake and other companions default to Heart (`af_heart`) unless
`LARK_TTS_VOICE` sets a different fallback. Retired UK voices resolve to
Emma or Fable without rewriting existing profiles.

Open **Menu → Stats** for a compact on-device HUD, or query `GET /api/diagnostics`
(or `lark status`) for device status, pipeline timings, and CPU and memory
usage for the server and host. The server totals include Kokoro. Firmware
reports ESP32-S3 CPU usage, free and minimum free heap, free PSRAM, Wi-Fi RSSI,
and uptime every five seconds. Turn diagnostics separate
OpenCode gate wait and first sentence, TTS queue and inference time, first frame
render, and elapsed time to the first queued PCM.

## Dedicated OpenCode

`lark-opencode.service` listens at `127.0.0.1:49375` with Basic auth.
Setup generates its password in the private `lark.env` and gives it separate
configuration, session data, cache, state, and workspace under
`~/.local/share/lark/opencode/` (or `$XDG_DATA_HOME`). It installs the model
fallback plugin, links the `lark` agent and legacy `auth.json`, and imports the
configured model providers' current V2 credentials once through OpenCode's
CLI. It does not share your session database or load personal plugins, MCP
servers, or coding `AGENTS.md`.
Setup does not edit personal OpenCode configuration. If upgrading, remove the
old `mcp.servers.lark` entry and `lark` agent symlink from your personal config.
Without `LARK_OPENCODE_URL`, Lark still discovers the shared service
through `service.json`. `LARK_OPENCODE_DIR` overrides the dedicated workspace.

### Companion models

Edit the dedicated `~/.local/share/lark/opencode/config/opencode/opencode.jsonc`
(under `$XDG_DATA_HOME` if set). Its model settings are:

```jsonc
{
  "agents": { "lark": { "model": "opencode/gemini-4-hydrogen#low" } },
  "plugins": [
    { "package": "./lark-model-router", "options": { "fallback": "opencode/claude-haiku-5-5" } }
  ]
}
```

Use `provider/model`, optionally with `#variant`, and keep both models on the
same provider: OpenCode only supports routing within a provider. Keep the other
config entries. Once OpenCode reloads the file, the next Lark turn uses the
new selection without rotating the companion's session or restarting services. Use
`/reload` in dedicated OpenCode if needed. Setup migrates an old
`LARK_OPENCODE_MODEL` on upgrade; after that, `lark.env` does not control
models. If you change providers, connect them on the dedicated server.

An explicit rate-limit or supported upstream rejection before output or tools
can retry once on the configured fallback. The next turn starts on the
primary. Stalls, interruptions and permission failures do not switch models.

## Local library

`./scripts/setup-host.sh --library` downloads and indexes the local text
library in the background: full English Wikipedia, Simple English Wikipedia,
and a pinned Project Gutenberg set weighted toward children's and classic
literature. It needs about 80 GB under `~/.local/share/lark/library/` and
writes progress to `~/.local/share/lark/library/setup.log`. Check status with
`~/.local/share/lark/lark library --status` after the build finishes.

`tools.lark.library` searches it in milliseconds, so the agent checks it before
web search for stories, books, and reference facts. Web search remains the path
for current information and anything the library misses. `lark library
--download --build` resumes interrupted downloads and rebuilds missing indexes;
use `--source enwiki|simple|gutenberg` to limit a run.

## Upgrading

Lark carries no runtime database migrations or backwards-compatibility shims.
Upgrades are documented as changelog entries in
[`.opencode/skills/lark-upgrade/references/changelog.md`](.opencode/skills/lark-upgrade/references/changelog.md)
and executed by OpenCode using the
[`.opencode/skills/lark-upgrade/SKILL.md`](.opencode/skills/lark-upgrade/SKILL.md)
skill across host config, SQLite, and the live Lark API/MCP tools.

### Background builds

Say “Make me a dinosaur quiz,” “Edit this game to add a hard round,” or
“Give yourself a bigger smile.” The companion acknowledges immediately and stays
available for other requests. For edits, open the saved app first, or type its
slug in backticks: ``Edit the app `dinosaur-quiz` to add a hard round``.
If the target is unclear, the companion asks. Playing saved games, calls, intercom,
and other simple controls don't start builds.

The companion shows a **Ready** card and announces the result once when awake and
idle. Saved builds appear in **Menu → Games**; they never replace the running
app. Face changes apply only if the active form hasn't changed in the meantime. If
the saved app changed while the builder worked, it keeps the newer version.
Tap **Cancel build** on the progress card or say “Cancel the build.” Regular
**Stop** only stops the foreground turn or speech.

### Lark MCP

OpenCode runs the installed `lark mcp` stdio proxy, which forwards
requests to the server's authenticated `POST /mcp` on loopback. The proxy
reads the token from its environment or the private `lark.env`; the
OpenCode config contains only the executable and its argument, no secrets.
On server startup after a change to MCP schemas, instructions, the agent
prompt, or the runtime contract version, Lark reconnects `lark` MCP and
verifies that OpenCode loaded its catalog. While that refresh is pending,
model turns fail without dispatch; local history and device settings remain
available. Diagnostics distinguish a verified catalog (`current`) from a
failed attempt awaiting retry (`waiting`). Startup never compacts sessions or
sends model prompts.

Each companion's next user turn that needs the model replaces a stale or
unversioned binding lazily. A current binding also gets replaced if OpenCode's
preflight read definitively reports that exact session missing. History, saved
forms, companion IDs, and scoped links stay in Lark; Lark does not delete the old
OpenCode session. A replacement can see a short, quoted excerpt of recent
ordinary dialogue for that device, not tool calls or results. The excerpt
never reaches behind a chat reset or reassignment. For legacy bindings it
starts after the last recorded binding write; older chat remains visible but
may not be in the model's context.

Lark never replays an accepted prompt or one with unclear admission or
effects. It checks for a missing session again on a
later user turn. A generic error does not invalidate a binding, and an
unconfirmed prompt may not appear in history. This does not undo work already
dispatched or guarantee exactly-once behavior inside OpenCode. Timer wakes
do not repair stale or missing bindings.

Code Mode exposes the `tools.lark` MCP tools:

| Tool | Purpose |
| --- | --- |
| `state` | Read keyed companions, terminals, saved apps, timers, and scoped JSON. Select slices with `include`; add `messages` or `diagnostics` when needed. |
| `patch` | Update device settings, companion form (`companion`), memory, or a timer. Use `companion.save_profile:true` to save under the current name or `false` to shift without replacing a favorite. Memory batches commit atomically; `expected_version:0` requires an absent key. |
| `story` | Create, update, get, or delete a validated Twee 3 story. |
| `library` | Search, read, list, or import from the local text library (`simple`, `enwiki`, `gutenberg`); answers in milliseconds with no page fetch. |
| `app` | Show a declarative screen or SVG, run a template or JS app, save/load/delete by slug, or exit to the face. |
| `emit` | Send spoken intercom and a card, play notes, or play a named sound (`sfx` or `chime`). |

With `device_id`, `state` always returns `me.device`, `me.companion`, and
`me.active_app`. Read source from `me.companion.face_svg` or fetch saved app source
with `include_code:true`. Read persistent values as `memory[scope][key].value`.

The agent checks `library` first for stories, books, and reference facts, and uses OpenCode's `websearch` only when the library misses or for current information. There is no `wait_event` tool:
pointer events and bounded ticks run in the server without keeping an OpenCode turn open.
Lark uses the pure-Go `goja` sandbox to run JS that renders SVG. Apps can
target one player or share state across multiple devices with a separate SVG
view for each player. `initial_state` seeds `ctx.initial_state` (or the state
directly if there is no `init`). `state_patch` shallow-merges live state and
redraws all players without restarting. App state must fit in 64 KB of JSON.
`save_as` writes apps and drawings to SQLite's `apps` table; `load` reopens
them. `storeGet(key)` reads persisted state in `init(ctx)`
and `render(state, player)`; `storeSet(key, value)` is available only in event
hooks. Use `onDrag` and `onUp` alongside `onTap` for sliders and drawing in
`/app` or on boards with firmware `0.9.1` or newer. `sfx(name)`, `sfxAll(name)`,
and `emit.sfx` support `boing`, `pop`, `win`, `buzz`, `ding`, and `whoosh`.
In JS, `sfx` and `sfxAll` also accept chimes such as `tap`; unknown names are
silent. App launches return `layout_warnings` for controls that overlap the
Home button or clip on round displays. Custom exits can use `lark-exit`
or the legacy `lark-exit`.

Lark pairs tinted Storybook vectors with Oat chrome. The nine templates share
24 stickers; you can also show them with no JS or SVG:

```js
app({devices: device_id, screen: {title: "Night sky", body: [
  {type: "sticker", name: "star", size: 72, palette: "amber"},
  {type: "text", text: "Wish on a star!"}
], actions: [{id: "wish", label: "Wish", variant: "primary"}]}, save_as: "night-sky"})
```

`screen` also supports `badge`, `bg`, `accent`, meters (`label`, `value`, `max`)
and grids of `{id, label, sprite?, sub?, active?, variant?}` items. It wraps
text and fits controls above Home on both displays. For interactive behavior,
return `ui.screen(spec)` from JS `render` and handle IDs in `onTap`.
`ui.sticker(name, {x, y, size, palette, id})` returns a Storybook SVG fragment.
`ui.sprite(rows, {x, y, scale, palette, id})` renders full rows as a crisp pixel
fragment. The `screen` sprite block also takes full rows.
`ui.palette` exposes the read-only palette (`.` is transparent;
`k w p m l s y r g a b n d c o` are colors).
Boards blend warm dawn and dusk and a cooler night using the device's Parent
timezone (or server local time); the web shell uses the browser clock.
Photo screens keep their original colors.

Use `app = ui.pagedText({title, text})` for wrapped reading with built-in
navigation, or return `ui.choices({title, prompt, choices})` from `render`
for a card or quiz with up to four buttons. Both leave room for Home and
fit the round screen. `ui.wrap` and `ui.esc` are available for custom SVG.
Set `timer.app` to a saved app name to launch it on the connected device when
the reminder goes off. See the [agent guide](agents/lark.md) for examples.

**One Voice Rule:** Only assistant prose is spoken as the reply, not tool
output or reasoning. Lark checks the streamed speech against the final
reply. The prompt starts with a short identity and source header, plus setup
or personality hints when applicable. The agent uses `state` for live context.

## Server data

Lark uses native SQLite (`go-sqlite3`) in WAL mode with split reader/writer pools. Persistent state lives
in `~/.local/share/lark/lark.db` (or `$XDG_DATA_HOME/lark/lark.db`);
`LARK_DB_PATH` overrides it. Keep the data directory private (0700, database
0600). The first boot imports valid rows from the six legacy JSON files in that
directory in one transaction and never modifies those backups.
Lark also writes private daily snapshots to `backups/lark-YYYY-MM-DD.db`
under its data directory, keeps the seven latest, and reports the latest in
**Diagnostics**.
Companions are stored in `companions` and attached terminals in `terminals`.
Shift instantly with `patch({device_id, companion: {preset: {species: "owl",
palette: "lavender", accessory: "scarf"}, save_profile: false}})`. Ten species
share eight colorways: peach, mint, lavender, sage, berry, amber, sky and
midnight. Set `eyes`, `markings` or a palette object with `base` and hex tints
to customize. Vector faces still support `face_edits` and `face_svg`. Existing
saved artwork is unchanged.

For bespoke creatures, costumes or styles use `companion.portrait: {prompt, bg?, edit?, style?}`. The dedicated
OpenCode service sends a billed request to `opencode/gemini-nano-banana-2.1`
using its OpenCode Console connection. OAuth needs no separate key, and this
request creates no OpenCode session. If `OPENCODE_API_KEY` is set in the server
environment, Lark calls Console directly instead. `edit:true` sends the
current sheet back as reference.
Lark aligns four poses from a 2×2 sheet and uses them for the six face
states. `style` defaults to `"storybook"`; `"pixel"` snaps each pose to a
64×64 grid and upscales without smoothing. Only SVG references go into the database; private PNGs under the
local `portraits/` and `blobs/photos/` directories are stored on disk. For local, no-cost art, use
the instant vector companion.
The canonical `messages` table drives browser history. The v2 migration
backfills it from legacy chat events and OpenCode session history.
New rows contain user text, final companion replies, intercom, and display cards,
not tool traces or prompt headers.

## API

Authenticated HTTP requests require `X-Lark-Token` (`/health` is unauthenticated;
`/mcp` and `/debug/pprof/...` are loopback + token only).
Devices connect over TCP and UDP on `LARK_DEVICE_PORT` (default `8766`) for
binary RGB565/RLE565/LZ4/JPEG frames, touch events, and 16 kHz signed 16-bit
mono PCM audio. The bridge pads voiced whole-clip STT with 150 ms of silence on
each side. Kokoro starts synthesizing sentences while OpenCode streams its reply
over SSE.

| Lark · port 8765 | Request → response |
| --- | --- |
| `GET /health` | STT engine/readiness, OpenCode, TTS, and Delta Chat readiness |
| `GET /api/diagnostics` | Device telemetry, pipeline timings, Kokoro readiness, server and host resource usage, and MCP contract refresh state (`mcp_contract_refresh`) |
| `GET /api/companions` | List all companions (`id`, `name`, `voice`, `online`, `dormant`, `terminals[]`) |
| `POST /api/companions` | Create a companion (`{}` creates `"New Companion"`) |
| `GET /api/companions/{id}`, `PATCH /api/companions/{id}`, `PUT /api/companions/{id}`, `DELETE /api/companions/{id}` | Read, update, or delete a companion (delete requires zero attached terminals) |
| `POST /api/companions/{id}/voice` | `{"voice":"af_heart"}` → saved `companion_id` and `voice` |
| `POST /api/companions/{id}/reset` | Restore the starter and start a fresh session (pass `{"archive_history":false}` to keep chat history) |
| `GET /api/companions/{id}/dc` | Delta Chat account ID and verified peer companion IDs |
| `GET /api/terminals` | List all terminals |
| `GET /api/terminals/{id}`, `PATCH /api/terminals/{id}`, `DELETE /api/terminals/{id}` | Read, update terminal volume/attachment, or delete a terminal |
| `POST /api/terminals/{id}/attach`, `POST /api/terminals/{id}/detach` | Attach a terminal to `{"companion_id":"..."}` or detach it |
| `POST /api/terminals/{id}/volume` | `{"auto":true}` or `{"auto":false,"volume":55}` |
| `POST /api/terminals/{id}/pair` | Generate and push a Delta Chat QR pairing card (`image/png`) to an attached terminal |
| `GET /api/terminals/{id}/hits` | Live screen state, revision, and touch hit targets for a connected terminal |
| `POST /api/tts` | `{"text":"Hello!"}`, optional `voice` → WAV audio (`audio/wav`) |
| `POST /mcp` | Loopback-only MCP JSON-RPC endpoint used by `lark mcp` |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/lark.bin` with SHA-256 |
| `GET /ota/box3/version.json`, `GET /ota/box3/latest.bin` | BOX-3 manifest or `firmware/build-box3/lark_box3.bin` with SHA-256 |

Use `lark status`, `lark companions`, `lark terminals`, `lark sim`, `lark device`,
`lark ota`, and `lark mcp` from the CLI to inspect and control the server.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `connected` |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull or upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Networking

Devices need TCP and UDP **8766** (`LARK_DEVICE_PORT`) for the native device protocol and TCP **8765** for OTA pull; direct OTA and `lark device` use TCP
**80** to the device. Use Avahi (`avahi-resolve -n lark-XXXX.local`) for
mDNS or the device IP if the container cannot resolve `.local`. HTTP and device
transport are unencrypted; use a trusted LAN and keep configs and firmware images private.

## License

Lark's source code is [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for
third-party software and model licenses.
