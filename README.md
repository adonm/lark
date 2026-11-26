# Lark

[Lark](https://github.com/adonm/lark) is a tap-to-talk companion with interactive stories, voice notes, and intercom. It starts as a friendly cat, then can shift into any creature or character a child imagines.

```text
Device  <-- TCP/UDP :8766: binary frames, touch, RTP PCM --> lark-edge (Go)
                                                              |  Parakeet STT, Silero VAD,
                                                              |  Smart Turn, Kokoro TTS
                                                              |  Unix socket, NDJSON
                                                              v
                                                           lark-brain (Node 26 TS)
                                                              |-- HTTP :8765 (admin, OTA, /remote)
                                                              |-- OpenCode in-process
                                                              `-- deltachat-rpc-server per companion
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for the component and data map, the
voice-turn budget, and the invariants an agent must not break.

**Targets:** ESP-VoCat v1.2 and ESP32-S3-BOX-3. Both use a 360×360 logical canvas,
centered on the BOX-3’s rectangular display. Lark renders RGB565 frames for
the boards. Boards draw a fallback face offline.

## Quickstart

1. On Linux, install Go 1.26, Node 26 with npm, GCC, a C++17 compiler, CMake
   ≥3.19, Ninja, Vulkan drivers and development headers/loader, `glslc`, the
   SPIR-V Headers CMake package, Python 3.11+, Podman, `ffmpeg`, `uvx` (USB
   flashing only), and `curl`. Use a systemd user session.

   ```sh
   ./scripts/setup-host.sh
   ```

   Setup compiles `lark-edge` with in-process Vulkan STT to
   `~/.local/share/lark/lark-edge`, runs `npm ci` for the TypeScript brain,
   SHA-256-pins the Parakeet Q8_0 GGUF, Silero v5, and FP32
   `kokoro-multi-lang-v1_0` pack, downloads `deltachat-rpc-server`, and installs
   and starts `lark-edge.service` and `lark-brain.service`. It retires the v1
   `lark.service` and `lark-opencode.service` units. On a multi-GPU host, use
   `vulkaninfo --summary` and pass `--gpu-index INDEX` for the intended device.
   Set `TTS_MODEL_DIR` in `lark.env` to load the Kokoro pack from a different
   directory. Set `LARK_REPO_DIR` when launching outside the checkout to locate
   the agent prompt and remote assets (the units set it).

   Rebuild and restart from the checkout with
   `~/.local/share/lark/lark deploy`. It reuses the cached Parakeet build,
   reinstalls `lark-edge` atomically, refreshes the units, and restarts both
   services. Setup remains the first-install path.

   Voice models need a Vulkan GPU, not an X display. Lark recognizes English
   without custom vocabulary. On firmware that advertises
   `caps.bridge_endpoint`, the edge listens through thinking pauses and ends the
   turn on gated silence, using VAD plus an adaptive room floor rather than
   transcript growth. Older clients keep their local VAD. You can still Send
   or Cancel while the mic is open. See
   [endpoint policy and replay results](docs/endpointing.md) and
   [STT measurements and artifact pins](docs/stt-speed.md).

2. Create the private firmware config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set `CONFIG_LARK_WIFI_SSID`, `CONFIG_LARK_WIFI_PASSWORD`,
   `CONFIG_LARK_URL` (native protocol, `lark://<lark-host>:8766`, no trailing
   slash), and `CONFIG_LARK_TOKEN` (copy `LARK_TOKEN` from
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

   Builds are incremental: `ccache` lives in `~/.cache/lark-ccache` and the
   ESP-IDF build tree stays in the repo, so a rebuild after a source edit takes
   seconds. Unchanged sources print `up to date` and exit immediately;
   `--force` rebuilds.

4. Update via `~/.local/share/lark/lark ota <ip|host>`; add `--no-build` to reuse the
   image, and prefix with `BOARD=box3` for BOX-3. The command refuses a board
   mismatch. Direct OTA works without the brain and reads the token from the
   environment or `lark.env`. The board confirms the new image after display
   initialization and Wi-Fi DHCP; the command checks the running partition.
   Builds use `lark` and `lark_box3` as the firmware project names.

5. Run `./scripts/check.sh` for ShellCheck, TypeScript typecheck and brain
   tests (including the real edge ↔ brain cross-process test), Python setup
   tests, Go vet and race tests, strict GCC/ASan/UBSan host tests for both
   board layouts, and tracked-file hygiene. GitHub Actions runs this offline
   without model weights or a Vulkan GPU. A separate job builds both ESP-IDF
   images on pushes to `main` and nightly.

   For the inner loop, `./scripts/check.sh --fast` builds, vets, and runs only
   the areas related to the current diff (~10 s) and prints what it chose.
   See [docs/testing.md](docs/testing.md) for per-area checks, flags and
   measured timings. `--remote-ui` adds the headless-Chromium remote-page tests
   (needs `playwright` or `playwright-core`; set `LARK_PLAYWRIGHT_MODULE` if it
   is not in a standard location). `scripts/setup-host.sh --models-only`
   installs the pinned Kokoro, Silero, and Smart Turn assets without replacing
   either service.

## Controls and voice notes

### Using a companion

- Tap the companion's face and speak after the tone. Pause to send
  automatically, or tap **Send** to finish early, **Cancel** to discard, or
  **Stop** to interrupt a reply.
- The display dims after 120 seconds idle. Tap the touchscreen to wake it.
- The on-device **Menu** provides touch controls:
  **Memories** shows the companion's saved memories, **Stories** opens the
  Twee reader, **Talk to…** opens intercom, **Volume** sets the level,
  **Voice** pages through the eight curated voices, and **Stats**
  opens the diagnostics HUD. **Home** returns to the companion.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” Lark bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or choose a peer in
  **Menu → Talk to…** to record directly to that companion.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  `lark_emit` to deliver a card and synthesized speech to the recipient.

### Voices and diagnostics

Companions choose from eight warm, kid-friendly storyteller voices:
Heart, Bella, and Puck (US English), or Fable, Lily, George, Alice, and
Isabella (UK English). The choice is saved per companion in the Lark database
and validated strictly: unknown ids are rejected and fall back to the
companion's theme voice. Mint defaults to Fable (`bm_fable`). Cake and other
companions default to Heart (`af_heart`).

Open **Menu → Stats** for a compact on-device HUD, or query `GET /api/diagnostics`
for device status and pipeline timings. `lark status` summarises health, models,
library, companions, and terminals. `lark doctor` checks the edge socket, the
HTTP API, model states, Delta Chat readiness, and orphaned accounts, and exits
non-zero on failures. Firmware reports ESP32-S3 CPU usage, free and minimum free
heap, free PSRAM, Wi-Fi RSSI, and uptime every five seconds.

`lark capture <companion> [seconds]` records the companion's voice-owner
microphone through the ordinary listening path into a 16 kHz mono WAV under
`~/.local/share/lark/debug/` (directory `0700`, file `0600`). Captures are never
uploaded to a third party; the holder of a scoped link can play, transcribe,
and delete them. They are never used as fixtures. The `/remote` Diagnostics tab
does the same from a phone.

## Remote controls

Send `/remote` to a companion from a verified Delta Chat contact and it replies
with one tappable link. The link opens a phone-first control page over an
OpenTunnel (`opentunnel.xyz`) subdomain and carries a token scoped to that
companion and contact. The page has live companion state, Say, a listening
on/off toggle, per-terminal volume, story progress with Continue and Start over,
history with filters and cursor paging, voice selection with an on-device
preview, and a Diagnostics tab for opt-in microphone captures (15/30/60 s) with
playback and transcription. History exposes in-person room messages and that
contact's whole Delta Chat conversation, including the companion's replies;
other contacts stay hidden. Captures are room-wide and labelled as such.
The page installs to a home screen with the companion's name and portrait.

Each `/remote` request mints a fresh token and invalidates the previous one for
that contact; `/remote off` revokes it. Tunnels start lazily on the first
`/remote` and are remembered in `meta`, so saved links keep working after a
restart. `lark tunnels` lists them and `lark tunnels stop <companion>` removes
one. If the `opentunnel` binary is missing, `/remote` replies with an install
hint instead of a link.

The page uses one REST route per command under `/remote/api/...` and one
Server-Sent Events stream at `/remote/events` for live state. There is no
WebSocket and no custom framing; each request authenticates with the scoped
token.

## OpenCode and models

The brain embeds `@opencode/sdk` in its own process. It keeps OpenCode's
configuration, session data, cache, and state under
`~/.local/share/lark/opencode/`, loads only the `lark.context`,
`lark.model-fallback`, and `lark.tools` plugins, and never reads your personal
OpenCode sessions, plugins, MCP servers, or coding `AGENTS.md`. Setup copies
the `opencode` provider entry from your OpenCode `auth.json` once; providers you
authenticate later can be copied again by rerunning `scripts/setup-host.sh`.

Model choices are code defaults in `brain/src/opencode.ts`:

| Role | Model |
| --- | --- |
| Companion (`lark`) | `opencode/claude-haiku-5-5#none` |
| Fallback | `opencode/gpt-6-luna#low` |
| Deep subagent (`deep`) | `opencode/claude-sonnet-5-5#high` |

An explicit rate-limit or supported upstream rejection before output or tools
can retry once on the fallback. The next turn starts on the primary. Stalls,
interruptions and permission failures do not switch models.

## Agent tools

The agent gets six native tools from `lark.tools`:

| Tool | Purpose |
| --- | --- |
| `lark_state` | Read keyed companions, terminals, timers, contacts, memory, and scoped JSON. Select slices with `include`. |
| `lark_patch` | Update terminal volume/sleep, companion name/form/voice/portrait/listening, memory, or a timer. Memory batches commit atomically. |
| `lark_story` | Create, update, get, or delete a validated Twee 3 story. |
| `lark_library` | Search, read, list, or import from the local text library (`simple`, `enwiki`, `gutenberg`); answers in milliseconds with no page fetch. |
| `lark_emit` | Send spoken intercom and a card, play notes, or play a named sound (`sfx` or `chime`). |
| `lark_audio` | Sample-exact metronome, short hum/chime/pluck tune, key-free identification of nearby music, or stop. |

`lark_audio.identify` records a 7 s 16 kHz WAV, fingerprints it locally with the
pinned MIT `shazamio-core` WASM module, asks Shazam's public discovery endpoint
once, and deletes the capture in `finally`. No API key or extra binary is
needed; `lark doctor` fails if the recognizer cannot load.

With `companion_id`, `lark_state` returns `me.companion`, `me.terminals`, and
`me.contacts`. Read persistent values as `memory[scope][key].value`; write them
through `lark_patch`'s `memory` argument. The agent checks `lark_library` first
for stories, books, and reference facts, and uses OpenCode's `websearch` only
when the library misses or for current information.

**One Voice Rule:** Only assistant prose is spoken as the reply, not tool
output or reasoning. Each turn starts with a `[Speaker: …]` or `[Event: timer]`
header and ends with one `[state: …]` line. Companion identity is the session
instruction entry, written at session create and refreshed when the name or
personality changes. The agent uses `lark_state` for live context.

## Local library

The planner chooses packs that fit the data volume, smallest first: Simple
English Wikipedia, Project Gutenberg, then full English Wikipedia only when the
volume can hold it. It keeps 20% of the data volume free and leaves a further
10% margin, so a full Wikipedia index appears only on disks with room for
roughly 65 GiB more.

`lark status` shows `installed`, `building (pct)`, `pending`, or
`skipped (space)` per pack plus the volume line and budget. `lark library
--plan` prints the same decision as JSON. Searches run in milliseconds through
`node:sqlite` FTS5, so the agent checks the library before web search for
stories, books, and reference facts; web search remains the path for current
information and anything the library misses.

Provisioning is explicit and runs in the compiled `lark-library` provisioner:

```sh
lark library --download --build
```

Use `--source enwiki|simple|gutenberg` to limit a run; interrupted downloads
resume. Downloaded dumps are deleted after indexing.

## Upgrading

Lark carries no backwards-compatibility shims at runtime. The v1 database opens
unchanged: `brain/src/db.ts` creates the same schema, so the cutover has zero
schema migration and zero firmware flash. Every breaking database, config,
protocol, or API change after that is documented as a changelog entry in
[`.opencode/skills/lark-upgrade/references/changelog.md`](.opencode/skills/lark-upgrade/references/changelog.md)
and executed by OpenCode using the
[`.opencode/skills/lark-upgrade/SKILL.md`](.opencode/skills/lark-upgrade/SKILL.md)
skill across host config, SQLite, and the live Lark API.

## Server data

The brain uses Node's built-in `node:sqlite` in WAL mode. Persistent state lives
in `~/.local/share/lark/lark.db`; `LARK_DB_PATH` overrides it. Keep the data
directory private (0700, database 0600). Companions are stored in `companions`
and attached terminals in `terminals`. A companion is its name, `form`, voice,
listening on/off, and an optional portrait. Changing `form` regenerates the
pixel-art background; the four poses come from one 2×2 portrait sheet.

For bespoke creatures, costumes, or styles use `companion.portrait: {prompt,
edit?}`. The brain calls `gemini-nano-banana-2.1` through OpenCode's inference
endpoint using your OpenCode Console connection. OAuth needs no separate key,
and this request creates no OpenCode session. If `OPENCODE_API_KEY` (or
`LARK_IMAGE_API_KEY`) is set in the environment, Lark calls Console directly
instead. `edit:true` sends the current sheet back as reference. Lark aligns
four poses from the sheet and uses them for the six face states. Private PNGs
live under the local `portraits/` directory; the database stores only ids and
metadata.

The canonical `messages` table drives device and remote history. New rows
contain user text, final companion replies, intercom, and display cards, not
tool traces or prompt headers.

## API

Authenticated HTTP requests require `X-Lark-Token` (`/health` is unauthenticated;
the remote page and its SSE stream use scoped tokens instead). Devices connect
over TCP and UDP on `LARK_DEVICE_PORT` (default `8766`) for binary
RGB565/RLE565/LZ4/JPEG frames, touch events, and 16 kHz signed 16-bit mono PCM
audio. The edge pads voiced whole-clip STT with 150 ms of silence on each side.
Kokoro starts synthesizing sentences while OpenCode streams its reply.

| Lark · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Edge model states, OpenCode, TTS, and Delta Chat readiness |
| `GET /api/library` | Offline content library plan: installed packs, current build, pending or space-skipped packs, and free/budget bytes |
| `GET /api/diagnostics` | Live router state plus Delta Chat readiness and orphaned accounts |
| `GET /api/companions` | List all companions (`id`, `name`, `voice`, `online`, `dormant`, `terminals[]`) |
| `POST /api/companions` | Create a companion (`{"name":"Mint","form":"a fox"}`) |
| `GET /api/companions/{id}`, `PATCH /api/companions/{id}`, `PUT /api/companions/{id}`, `DELETE /api/companions/{id}` | Read, update, or delete a companion |
| `GET /api/terminals` | List all terminals |
| `GET /api/terminals/{id}`, `PATCH /api/terminals/{id}`, `DELETE /api/terminals/{id}` | Read, update volume/attachment, or delete a terminal |
| `POST /api/terminals/{id}/attach`, `POST /api/terminals/{id}/detach` | Attach a terminal to `{"companion_id":"..."}` or detach it |
| `POST /api/terminals/{id}/volume` | `{"volume":55}` |
| `GET /api/terminals/{id}/hits` | Live screen state, revision, and touch hit targets for a connected terminal |
| `POST /api/capture` | `{"companion_id":"cake","seconds":20}` → raw RTP capture saved as a 16 kHz mono WAV under `~/.local/share/lark/debug/` (host-local, `0700`, never synced) |
| `GET /api/remote/tunnels`, `DELETE /api/remote/tunnels/{companion}` | List or stop saved scoped remote tunnels (no tokens exposed) |
| `GET /remote?token=...` | Scoped control page (also served through the companion's OpenTunnel) |
| `GET /remote/events?token=...` | Server-Sent Events stream for live state and story progress |
| `POST /remote/api/say`, `listen`, `volume`, `voice`, `story/open`, `story/restart`, `revoke` | One scoped JSON command per route; `revoke` drops every token and tunnel for that companion |
| `GET /remote/api/history?cursor=...` | Scoped history page with channel, search, and cursor parameters |
| `POST /remote/api/capture` | `{"seconds":15}` → start an opt-in microphone capture on the companion's voice owner; `409` while one is running |
| `GET /remote/api/captures` | Scoped list of local captures with size, duration, transcript, and computing flag |
| `POST /remote/api/captures/{name}/transcribe`, `GET /remote/api/captures/{name}.wav`, `DELETE /remote/api/captures/{name}` | Transcribe to a `.txt` sidecar, play the WAV with range support, or delete both files |
| `GET /ota/...` | Firmware files published under `$LARK_DATA_DIR/ota/` |

Use `lark status`, `lark doctor`, `lark companions`, `lark terminals`,
`lark tunnels`, `lark capture`, `lark library`, `lark ota`, and `lark deploy`
from the CLI. `lark` routes HTTP commands to the brain, `ota` to `lark-edge`,
and `library --download|--build` to `lark-library`.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `connected` |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull or upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Networking

Devices need TCP and UDP **8766** (`LARK_DEVICE_PORT`) for the native device
protocol and TCP **8765** for OTA pull; direct OTA uses TCP **80** to the device.
Use Avahi (`avahi-resolve -n lark-XXXX.local`) for mDNS or the device IP if the
container cannot resolve `.local`. HTTP and device transport are unencrypted;
use a trusted LAN and keep configs and firmware images private.

## License

Lark's source code is [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for
third-party software and model licenses.
