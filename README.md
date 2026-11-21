# Lark

[Lark](https://github.com/adonm/lark) is a tap-to-talk companion with interactive diagrams, voice notes, live calls, and a browser mirror. It starts as a friendly cat, then can shift into any creature or character a child imagines.

```text
Device  <-- TCP/UDP :8766: binary frames, touch, RTP PCM --> Go bridge :8765 <-- /ws, /app: SVG --> Browser
                                              |-- Parakeet (in-process Vulkan speech to text)
                                              |-- OpenCode :49375 (loopback, reply deltas over SSE)
                                              `-- Kokoro   (FP32, embedded via sherpa-onnx)
```

**Targets:** ESP-VoCat v1.2 and ESP32-S3-BOX-3. Both use a 360×360 logical canvas,
centered on the BOX-3’s rectangular display. The bridge runs apps and renders
 RGB565 updates for hardware and SVG for `/app`. Boards draw a fallback face offline.

**New in 0.9.3:** Both boards use the touchscreen for all touch controls.
VoCat’s capacitive head pads are disabled.

**New in 0.9.2:** Live two-way **Call** and a paged voice picker in the
on-device picker. Firmware announces features in `hello` and flushes only the
dirty LCD columns within changed rows. The bridge and dashboard add paginated
chat history, faster face rendering, and separate turn and hardware diagnostics.

## Quickstart

1. On Linux, install Go 1.26, GCC, a C++17 compiler, CMake ≥3.19, Ninja,
   Vulkan drivers and development headers/loader, `glslc`, the SPIR-V Headers
   CMake package, Python 3.11+, OpenCode V2, Podman, `uvx` (USB flashing only),
   and `curl`. Use a systemd user session.

   ```sh
   ./scripts/setup-host.sh
   ```

   Setup builds the Go bridge with in-process Vulkan STT, installs the
    SHA-256-pinned Parakeet Q8_0 GGUF, Silero v5, and FP32 `kokoro-multi-lang-v1_0`
    pack, and starts the bridge and a dedicated OpenCode service. On a multi-GPU
   host, use `vulkaninfo --summary` and pass `--gpu-index INDEX` for the
   intended device. The bridge runs Kokoro in-process via
   `sherpa-onnx-go` and checksum-verified libraries in `$ORIGIN/lib`.
   The Vulkan loader and driver come from the OS. Host setup still uses
   Python; the bridge has no Python runtime or TTS subprocess. Set
   `TTS_MODEL_DIR` in `bridge.env` to load that pack from a different directory.
   Set `LARK_REPO_DIR` when launching outside the checkout to locate the
   agent prompt and OTA files (the systemd unit sets it). Choose the models in
    the [dedicated OpenCode config](#companion-models).

     STT needs a Vulkan GPU, not Hex or an X display. Lark recognizes
    English without custom vocabulary. On firmware 0.9.5 and `/app`,
    the bridge listens through thinking pauses and uses local transcription
    to decide when to send a turn. Clients without the capability keep their
    local VAD. You can still Send or Cancel while the mic is open. See
    [endpoint policy and replay results](bridge/turn-taking.md) and
    [STT measurements and artifact pins](bridge/stt-speed.md).

2. Create the private firmware config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set `CONFIG_LARK_WIFI_SSID`, `CONFIG_LARK_WIFI_PASSWORD`,
   `CONFIG_LARK_BRIDGE_URL` (`http://<bridge-host>:8765` or `lark://<bridge-host>:8766`, no trailing slash), and
   `CONFIG_LARK_BRIDGE_TOKEN` (copy `LARK_BRIDGE_TOKEN` from
   `~/.config/lark/bridge.env`, or use `$XDG_CONFIG_HOME`).
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

4. Update via `./scripts/push-ota.sh <ip|host>`; add `--no-build` to reuse the
   image, and prefix with `BOARD=box3` for BOX-3. The script refuses a board
   mismatch. Direct OTA works without the bridge and reads the token from the
    environment or `bridge.env`. The board confirms the new image after display
    initialization and Wi-Fi DHCP; the script checks the running partition.
   Builds use `lark` and `lark_box3` as the firmware project names.

5. Run `./scripts/check.sh` for ShellCheck (if installed), Node tests, Go vet
   and race tests, strict GCC/ASan/UBSan host tests for both board layouts,
   and tracked-file hygiene. GitHub Actions runs this offline without model
   weights or a Vulkan GPU; it does not build ESP-IDF images. Build those locally with the
   commands above (`BOARD=vocat` and `BOARD=box3`).

   Add `--live` to exercise the product with virtual companions and firmware
   clients for both board layouts. It builds a fresh loopback bridge with a
   temporary database, real Parakeet Vulkan, Silero and Kokoro, synthetic
   photos, and a deterministic model stub. It does not connect to the installed bridge, use
   model quota, or test wake word. Allow at least two minutes for the actual
   idle timeout. Set `LARK_PARAKEET_NATIVE` to the `native/` prefix from
   `scripts/build-parakeet.sh` to reuse its verified static libraries.
   `scripts/setup-host.sh --models-only` installs the pinned Kokoro and Silero
   assets without replacing the bridge or restarting services.

   `--e2e-real` uses the dedicated local OpenCode server, its configured companion
   model and fallback, and disposable sessions; it can spend model quota.
   `--deployed` runs the older deployed-service and OTA probes (no upload or
   flash).
   See [evals and real-model smoke](docs/evals.md)
   for optional tests against the configured model.

## Controls, voice notes, and calls

### Parent controls

Store a separate random token (at least 32 ASCII characters, no spaces) in
`~/.config/lark/parent.token` (or under `$XDG_CONFIG_HOME`), mode `0600`.
The bridge reads it at startup. On the LAN, open **Parent controls** in `/app`
and enter both tokens. Keep the parent token with the grown-ups; kids should
use their device or a scoped share link, not the LAN dashboard. Scoped links
cannot open the controls.

- Set daily quiet hours in the device's IANA time zone (for example,
  `America/New_York`). A window can cross midnight. A dim screen explains why
  the companion is unavailable. Calls and intercom are declined; due timers are
  skipped, not replayed.
- Set an optional daily time budget. Recording, chat and playback, visible
  apps, and connected calls share the budget. Concurrent activity counts once
  and the total resets at local midnight. The companion announces when time is up.
- Pause a device for a set time, or choose **Unlock and override** to allow use
  temporarily. Tapping cannot unlock it. Kids can ask “Pause for five minutes”
  to start a break (up to 30 minutes) after parent access is set up. Only a
  parent can unlock it early.

Rules follow the device, not the companion's form. The parent view shows
today's usage and seven-day activity counts; chat contents stay in History.
The agent can read its current policy but cannot edit it.

### Using a companion

- Tap the companion's face and speak after the tone. Pause 1.5 seconds to send
  automatically, or 3 seconds once you've spoken for 4 seconds. Tap **Send**
  to finish early, **Cancel** to discard, or **Stop** to interrupt a reply.
- The display dims after 120 seconds idle. Tap the touchscreen to wake it.
- The on-device **Menu** and `/app` live screen share the same touch controls.
  **Games** launches saved apps, **Memories** shows the companion's saved memories,
  and **History** shows the last 20 messages, newest first. Tap a row for details.
  **Talk to…** opens intercom, **Volume** sets the level or enables **Auto**,
  **Voice** pages through six curated voices, and **Stats** opens the diagnostics HUD.
  Auto-volume estimates ambient noise from recordings and adjusts the DAC in RAM
  without writing to flash. **Home** returns to the companion. Scoped tunnels don't
  expose Games, Memories, or Stats.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” The bridge bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or choose a peer in
  **Menu → Talk to…** to record directly to that companion.
- Tap **Call** on an incoming Intercom card, pick a peer in **Menu → Call**, or
  say “call \<name\>.” Both devices must be online and call-capable. The recipient
  can answer or decline; either side can mute its mic or hang up. Unlike a
  voice note, a call streams both ways in real time.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  `emit` to deliver a card and synthesized speech to the recipient.

## Browser dashboard

Open `http://<host-lan-ip>:8765/app` on a trusted LAN and enter the bridge
token. Select a physical device to mirror and control it, or use
**+ New Companion** to meet a virtual companion. The no-build UI ships with
the Go bridge: Oat supplies the controls and theme, native JavaScript handles
live screen and audio, and HTMX 4 swaps server-rendered Parent controls.

Say hi to the starter cat, then ask it to become a fox, owl, dragon, robot, or
anything you invent. Give it a name, colors, and a voice. Ask it to shift again
anytime or settle into a favorite so you can return to it later. Shifting keeps
the same conversation. Open **Reassign / Load Form** in the LAN sidebar to load
from **Saved Forms**, or move or swap an active companion, voice, and conversation
between devices. **New Chat** archives the current conversation and returns to
the starter. Shared links can chat but cannot reset or reassign companions.
Chat updates live over `/ws`; **Load older** fetches earlier messages across
sessions. Use **Call** from a virtual companion in `/app` to call a compatible board
or another virtual companion. Mirroring a physical device does not turn the browser into
its microphone. Use **Photo** to show an image on the selected companion's
screen and leave a card in its chat. Use **Voice** under the screen for one-click
Heart or Fable, or choose one of six warm, kid-friendly storyteller voices:
Heart, Bella, and Puck (US English), or Fable, Emma, and George (UK English).
The choice is saved per device in the bridge database. Mint defaults to Fable
(`bm_fable`). Cake and other companions default to Heart (`af_heart`) unless
`LARK_TTS_VOICE` sets a different fallback. Retired UK voices resolve to
Emma or Fable without rewriting existing profiles.
The conversation panel shows active timers for the selected companion with a
countdown and a Cancel button. The LAN sidebar's **Saved Memory & Assets**
section lets you inspect and delete saved keys and visuals.

Plain LAN HTTP supports the SVG mirror, text chat, and audio playback.
Browser calls and recording need microphone access (HTTPS or localhost).
Voice notes use the same adaptive silence timing as the boards. On manual stop, the browser flushes any buffered
audio before ending the recording, without a fixed recording tail. For a remote
phone, install the [OpenTunnel CLI](https://opentunnel.xyz) on the bridge host. If it is
outside the bridge service's `PATH` and `~/.local/bin`, set
`LARK_OPENTUNNEL_BIN` to its path in `bridge.env`. Restart the bridge
with `systemctl --user restart lark-bridge.service` so it finds the CLI.
In the dashboard, select a virtual companion, choose **Create Tunnel** to start
its route, then **Copy Share Link**. The HTTPS link opens the same companion and
conversation as the LAN dashboard, with mic, screen, voice notes, and calls to
online devices. The scoped token does not open the LAN dashboard, admin routes, or
other chats. Treat the link as a password and use **Close Tunnel** to revoke it.

### Install Phone Lark

Open the companion's HTTPS share link, or select a virtual companion in `/app` on a trusted
HTTPS LAN origin (or localhost). In **Devices**, **Join on Phone** selects a
browser-local companion instead; the shortcut opens that identity again. Ordinary LAN
HTTP shows the same phone layout but can't install the app or use the mic.

- **Android (Chrome):** tap **Install companion**, or use the browser menu's
  **Install app** / **Add to Home screen**.
- **iPhone/iPad (Safari):** tap **Share → Add to Home Screen**, then **Add**.
- **Desktop (Chrome/Edge):** click **Install companion** or the install icon in the address bar.

The shortcut opens the companion in a standalone window and uses its face as
the icon. A share-link install retains only its private access and stops working when the link is
revoked. A LAN install may ask for your bridge token if the browser uses
separate storage. The app shell can load offline; live chat, history, audio,
and photos still need the bridge.

On phones, the live face sits above chat and the composer. A virtual companion goes
offline after 45 seconds in the background or five minutes without interaction.
Ringing, calls, recording, thinking, and playback finish first. Tap
**Tap to reconnect** to unlock audio and resume; reconnecting won't take over
a companion already open elsewhere.

### Geeky diagnostics

Open **Menu → Stats** for a compact on-device HUD, or **Diagnostics** in the
LAN `/app` dashboard for device status, pipeline timings, and CPU and memory
usage for the bridge and host. The bridge totals include Kokoro. Firmware
reports ESP32-S3 CPU usage, free and minimum free heap, free PSRAM, Wi-Fi RSSI,
and uptime every five seconds over WebSocket. The dashboard also shows
WebSocket round-trip time.
Device status also reports the last frame revision and LCD flush duration,
input overflows, and PCM starvation/drop counts. Turn diagnostics separate
OpenCode gate wait and first sentence, TTS queue and inference time, first frame
render, and elapsed time to the first queued PCM (or HTTP audio URL). The board's
1.5–3 seconds of automatic-send silence happen before these bridge timings.

## Dedicated OpenCode

`lark-opencode.service` listens at `127.0.0.1:49375` with Basic auth.
Setup generates its password in the private `bridge.env` and gives it separate
configuration, session data, cache, state, and workspace under
`~/.local/share/lark/opencode/` (or `$XDG_DATA_HOME`). It installs the model
fallback plugin, links the `lark` agent and legacy `auth.json`, and imports the
configured model providers' current V2 credentials once through OpenCode's
CLI. It does not share your session database or load personal plugins, MCP
servers, or coding `AGENTS.md`.
Setup does not edit personal OpenCode configuration. If upgrading, remove the
old `mcp.servers.lark` entry and `lark` agent symlink from your personal config.
Without `LARK_OPENCODE_URL`, the bridge still discovers the shared service
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
`LARK_OPENCODE_MODEL` on upgrade; after that, `bridge.env` does not control
models. If you change providers, connect them on the dedicated server.

An explicit rate-limit or supported upstream rejection before output or tools
can retry once on the configured fallback. The next turn starts on the
primary. Stalls, interruptions and permission failures do not switch models.

## Upgrading

Lark carries no runtime database migrations or backwards-compatibility shims.
Upgrades are documented as changelog entries in
[`.opencode/skills/lark-upgrade/references/changelog.md`](.opencode/skills/lark-upgrade/references/changelog.md)
and executed by OpenCode using the
[`.opencode/skills/lark-upgrade/SKILL.md`](.opencode/skills/lark-upgrade/SKILL.md)
skill across host config, SQLite, and the live bridge API/MCP tools.

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
**Stop** only stops the foreground turn or speech. A companion can have one build at
a time. After a bridge restart, queued work can continue; in-flight work is
reported as interrupted.

Setup links the tool-free `lark-builder` agent in the dedicated OpenCode
configuration. The builder uses `agents.lark.model` by default. To use a
different model, add `agents.lark-builder.model` in that same JSONC file,
using the same `provider/model#variant` format. The bridge explicitly selects
both the agent and model for each disposable session and deletes it afterward.
The fallback router retains its existing foreground configuration.

### Pairing

Click **Pair OpenCode** in the LAN dashboard's Devices sidebar to create a
short-lived, single-use link. The link opens the dedicated server via the
`lark-opencode-lan.socket` TCP forwarder on port `49376`. It does not pair
with your personal OpenCode or work through companion share links. Keep ports `8765`
and `49376` on a trusted LAN; neither uses TLS. Pair only trusted clients: a
paired client gets access to the dedicated OpenCode server, not just companion chat.
To turn off LAN OpenCode access, stop both forwarder units, disable
`lark-opencode-lan.socket`, remove `LARK_OPENCODE_PAIR_PORT` from `bridge.env`,
and restart the bridge.

### Lark MCP

OpenCode runs the installed `lark-bridge mcp` stdio proxy, which forwards
requests to the bridge's authenticated `POST /mcp` on loopback. The proxy
reads the token from its environment or the private `bridge.env`; the
OpenCode config contains only the executable and its argument, no secrets.
On bridge startup after a change to MCP schemas, instructions, the agent
prompt, or the runtime contract version, Lark reconnects `lark` MCP and
verifies that OpenCode loaded its catalog. While that refresh is pending,
model turns fail without dispatch; local history and device settings remain
available. Diagnostics distinguish a verified catalog (`current`) from a
failed attempt awaiting retry (`waiting`). Startup never compacts sessions or
sends model prompts. `LARK_DEFER_MCP_COMPACTION` no longer has an effect.
The authenticated LAN `/api/opencode/compact` still supports explicit
compaction and MCP reconnects.

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

Code Mode exposes exactly four MCP tools under `tools.lark`:

| Tool | Purpose |
| --- | --- |
| `state` | Read keyed devices, companion forms, saved apps, timers, and scoped JSON. Select slices with `include`; add `saved_companions`, `messages`, or `diagnostics` when needed. |
| `patch` | Update device settings, companion form (`companion`), memory, or a timer. Use `companion.save_profile:true` to save under the current name or `false` to shift without replacing a favorite. Memory batches commit atomically; `expected_version:0` requires an absent key. |
| `app` | Show a declarative screen or SVG, run a template or JS app, save/load/delete by slug, or exit to the face. |
| `emit` | Send spoken intercom and a card, play notes, or play a named sound (`sfx` or `chime`). |

With `device_id`, `state` always returns `me.device`, `me.companion`, and
`me.active_app`. Read source from `me.companion.face_svg` or fetch saved app source
with `include_code:true`. Read persistent values as `memory[scope][key].value`.

The agent can also use OpenCode's `websearch`. There is no `wait_event` tool:
pointer events and bounded ticks run in the bridge without keeping an OpenCode turn open.
The bridge uses the pure-Go `goja` sandbox to run JS that renders SVG. Apps can
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
timezone (or bridge local time); the web shell uses the browser clock.
Photo screens keep their original colors.

Use `app = ui.pagedText({title, text})` for wrapped reading with built-in
navigation, or return `ui.choices({title, prompt, choices})` from `render`
for a card or quiz with up to four buttons. Both leave room for Home and
fit the round screen. `ui.wrap` and `ui.esc` are available for custom SVG.
Set `timer.app` to a saved app name to launch it on the connected device when
the reminder goes off. See the [agent guide](agents/lark.md) for examples.

**One Voice Rule:** Only assistant prose is spoken as the reply, not tool
output or reasoning. The bridge checks the streamed speech against the final
reply. The prompt starts with a short identity and source header, plus setup
or personality hints when applicable. The agent uses `state` for live context.

## Bridge data

The bridge uses native SQLite (`go-sqlite3`) in WAL mode with split reader/writer pools. Persistent state lives
in `~/.local/share/lark/lark.db` (or `$XDG_DATA_HOME/lark/lark.db`);
`LARK_DB_PATH` overrides it. Keep the data directory private (0700, database
0600). The first boot imports valid rows from the six legacy JSON files in that
directory in one transaction and never modifies those backups.
The bridge also writes private daily snapshots to `backups/lark-YYYY-MM-DD.db`
under its data directory, keeps the seven latest, and reports the latest in
**Diagnostics**.
Active and saved forms are stored in `companions` and `saved_companions`.
Shift instantly with `patch({device_id, companion: {preset: {species: "owl",
palette: "lavender", accessory: "scarf"}, save_profile: false}})`. Ten species
share eight colorways: peach, mint, lavender, sage, berry, amber, sky and
midnight. Set `eyes`, `markings` or a palette object with `base` and hex tints
to customize. Vector faces still support `face_edits` and `face_svg`. Existing
saved artwork is unchanged.

For bespoke creatures, costumes or styles use `companion.portrait: {prompt, bg?, edit?, style?}`. The dedicated
OpenCode service sends a billed request to `opencode/gemini-nano-banana-2.1`
using its OpenCode Console connection. OAuth needs no separate key, and this
request creates no OpenCode session. If `OPENCODE_API_KEY` is set in the bridge
environment, the bridge calls Console directly instead. `edit:true` sends the
current sheet back as reference.
The bridge aligns four poses from a 2×2 sheet and uses them for the six face
states. `style` defaults to `"storybook"`; `"pixel"` snaps each pose to a
64×64 grid and upscales without smoothing. Only SVG references go into the database; private PNGs under the
local `portraits/` and `blobs/photos/` directories are stored on disk. For local, no-cost art, use
the instant vector companion.
The canonical `messages` table drives browser history. The v2 migration
backfills it from legacy bridge chat events and OpenCode session history.
New rows contain user text, final companion replies, intercom, and display cards,
not tool traces or prompt headers.

## API

Authenticated HTTP requests require `X-Lark-Token` (and `X-Lark-Parent-Token`
for parent controls).
Hardware needs firmware `0.9.0` or newer for playback acknowledgments with an
audio ID, and announces `proto: 2` in its authenticated `/ws` `hello`.
Firmware `0.9.2` and newer report `caps` for battery, Call, and drag. Both
current boards use touchscreens, not head pads. The bridge remembers each
board there, using the socket IP and port 80
unless the hello specifies a port. The SVG renderer
streams dirty rectangles as `0x03` RGB565 binary frames, using RLE565 when
smaller. BOX-3 announces its native size; the bridge centers the square
artwork and maps raw touch coordinates back into SVG space. Browsers receive
`op: "svg"` with `device_id`; controls share the boards’ hit targets and also
work with Tab, Enter, and Space.

`0x01` note upload and `0x02` playback carry 16 kHz signed 16-bit
little-endian mono PCM. Clients with `caps.call_pcm` use `0x04` for 40 ms call
uploads when `call.start` sets `mic_prefix: 4`; legacy clients use `0x01`.
Voice capture and intercom use WebSocket. The bridge
pads voiced whole-clip STT with 150 ms of silence on each side. Kokoro starts
synthesizing sentences while OpenCode streams its reply over SSE. Firmware
`0.9.0` removes the legacy `/speak` endpoint.

| Bridge · port 8765 | Request → response |
| --- | --- |
| `GET /health` | STT engine/readiness, OpenCode, and TTS readiness |
| `GET /ws`, `GET /app` | WebSocket hub and embedded browser UI |
| `GET /api/diagnostics` | LAN-only device telemetry, WebSocket RTT, pipeline timings, Kokoro readiness, bridge and host resource usage, and MCP contract refresh state (`mcp_contract_refresh`) |
| `POST /api/diagnostics/stt` | Loopback only: synthesize and transcribe a fixed test phrase without a session or playback |
| `GET /api/devices` | Remembered devices with `device_id`, `character`, `companion`, `voice`, and current `ws_connected`; hardware also includes `ip`, `port`, and `last_seen` |
| `GET /api/scope` | Current scope and ordered `voices` catalog |
| `POST /api/virtual-clients` | `{}` creates a starter companion; optional legacy `character` and `theme` seed a form |
| `GET /api/companions`, `DELETE /api/companions/{slug}` | List or delete LAN-only saved forms |
| `POST /api/companions/reassign` | LAN-only `from_device_id`, `to_device_id`, and `mode` (`swap` or `move`) → transfer companions, voices, and sessions |
| `POST /api/devices/{id}/companion` | Update active form fields or send `{"slug":"cake"}` to load a form (LAN-only) |
| `GET /api/devices/{id}/messages`, `GET /api/history?device_id={id}` | Canonical chat, newest page first and chronological within each page. `limit` defaults to 100 (max 500). If `has_more`, pass `next_cursor.before_ts` and `next_cursor.before_id` as query parameters to load older messages. |
| `POST /api/devices/{id}/voice` | `{"voice":"af_heart"}` → saved `device_id` and `voice`; scoped links can change only their own companion |
| `POST /api/chat` | `{"text":"Explain a circle","device_id":"my-lark"}` → accepted on a connected device; otherwise returns `reply`, `session_id`, and `audio_url` (LAN only) |
| `GET /api/audio/<id>` | Reply PCM from `audio_url`; streams by sentence while synthesis runs |
| `POST /api/tts` | `{"text":"Hello!"}`, optional `device_id` → complete PCM clip using that device's voice |
| `POST /api/reset` | `{"device_id":"my-lark"}` → archive the active form if not already saved, restore the starter, and start a fresh session |
| `POST /api/opencode/compact` | LAN-only; optional `device_id` selects one session (default: all), optional `reconnect_mcp: true` refreshes MCP first → `ok`, `compacted`, and `skipped` device IDs |
| `GET /api/memory?scope=…`, `GET /api/memory/scopes`, `DELETE /api/memory?scope=…&key=…` | LAN-only saved JSON and populated scopes |
| `GET /api/apps`, `DELETE /api/apps/{name}`, `POST /api/apps/run` | List, delete, or run saved apps and drawings (LAN only) |
| `GET /api/timers?device_id=…`, `DELETE /api/timers/{id}` | List/cancel active timers; scoped links can see and cancel only their own companion's timers |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/lark.bin` with SHA-256 |
| `GET /ota/box3/version.json`, `GET /ota/box3/latest.bin` | BOX-3 manifest or `firmware/build-box3/lark_box3.bin` with SHA-256 |

Use the exact `device_id` in chat requests. To send an intercom without an
agent turn, say or type “Tell Mint dinner is ready,” substituting a nearby
companion's name. Intercom requires a connected recipient.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `connected` |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull or upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Networking

Devices need TCP and UDP **8766** (`LARK_DEVICE_PORT`) for the native device protocol and TCP **8765** for OTA pull; browsers need TCP **8765** to the bridge; direct OTA uses TCP
**80** to the device. Use Avahi (`avahi-resolve -n lark-XXXX.local`) for
mDNS or the device IP if the container cannot resolve `.local`. HTTP and WS
are unencrypted; use a trusted LAN and keep configs and firmware images private.

## License

Lark's source code is [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for
third-party software and model licenses.
