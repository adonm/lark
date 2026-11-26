# Architecture

A map of Lark for a new contributor or an OpenCode agent working in this repo.
Every number here was measured on the reference host (16-core CPU, one Vulkan
GPU, NVMe); treat small differences as noise.

Lark v2 is two processes. A compiled Go **edge** owns hardware, audio, and
speech models. A Node 26 TypeScript **brain** owns conversation state,
companions, Delta Chat, stories, and OpenCode. They speak one versioned NDJSON
protocol over a Unix socket, so either side can restart without dropping the
other's hardware session.

## Components

**`lark-edge` — Go, real-time media edge.** Binds the device listener on TCP
and UDP `:8766`, the brain IPC socket (`$LARK_DATA_DIR/edge.sock`, `0600`), and
loads Parakeet STT, Silero VAD, Smart Turn, and Kokoro TTS in the background.
It owns the terminal protocol (binary frames in RGB565/RLE565/LZ4/JPEG,
touch events, control messages, 16 kHz mono s16le PCM), endpointing, capture
DSP, playback queueing, and the OTA push command. `lark-edge library` also runs
the bulk Wikipedia/Gutenberg downloads and FTS5 index builders, which need a
compiled SQLite driver. STT needs a Vulkan GPU; Kokoro and Silero run through
sherpa-onnx and the checksummed libraries in `$ORIGIN/lib`.

On start the edge prints `lark-edge terminal=… udp=… brain=…`. Every brain
connection immediately receives `ready` (model states) plus one
`terminal.connected` snapshot per live board, so a brain restart never
disturbs a hardware TCP session. `/health` exposes the same model states.

**`lark-brain` — Node 26 TypeScript.** Owns HTTP `:8765` (admin API, OTA files,
remote page), the household SQLite database through `node:sqlite` in WAL mode,
one `deltachat-rpc-server` child per companion, timers, portraits, the library
reader/planner, story parsing, and the embedded OpenCode runtime from
`@opencode/sdk`. OpenCode runs in-process with its own config, session data,
cache, and state under `$LARK_DATA_DIR/opencode/`; it loads only Lark's
`lark.context`, `lark.model-fallback`, and `lark.tools` plugins. One long-lived
session per companion.

The agent gets these native tools:

| Tool | Purpose |
| --- | --- |
| `lark_state` | Read companions, terminals, timers, contacts, memory, and scoped JSON. |
| `lark_patch` | Update terminal volume/sleep, companion name/form/voice/portrait/listening, memory, timers. |
| `lark_story` | Create, update, read, or delete a validated Twee 3 story. |
| `lark_library` | Search/read/list/import from the local text library. |
| `lark_emit` | Intercom, speak on a terminal, play notes or a named sound. |

A `deep` subagent (`opencode/claude-sonnet-5-5#high` by default) handles
multi-step plans, hard questions, and stories. The primary companion model is
`opencode/claude-haiku-5-5#none`; the fallback retries once on
`opencode/gpt-6-luna#low` for a rate limit or supported upstream rejection. When
the primary accepts a prompt but emits no token for 8 s, Lark interrupts it and
retries once on that fallback model too.

**Delta Chat.** One `deltachat-rpc-server` child and one Chatmail account per
companion. Contacts pair by scanning the companion's QR on the device screen.
Commands: `/say`, `/private`, `/help`, `/story`, `/stories`, `/remote` (and
`/remote off`). Every companion has its own address; Lark keeps verified
contacts and routes intercom through the Chatmail relay. Off-device messages
are text-only: incoming voice notes are decoded with ffmpeg and transcribed,
while replies and spoken intercom clips are transcribed and sent as text. No
outbound Opus attachment is created.

**Library.** Three SQLite FTS5 packs, planned against the data volume: Simple
English Wikipedia, Project Gutenberg, then English Wikipedia when the volume can
hold the peak download plus index. The brain reads, searches, and plans with
`node:sqlite`; the compiled `lark-edge library --download --build` downloads
and builds the indexes. Search returns in tens of milliseconds, so the agent
checks it before web search.

**`/remote`.** A phone-first control page served at `/remote?token=…` and
through an OpenTunnel subdomain. It ships no framework bundle: Oat's theme
variables and local CSS style the page. REST commands live under
`/remote/api/…`; live state is one Server-Sent Events stream at
`/remote/events`. A token is scoped to one companion and one contact, minted
per request, and revoked by `/remote off` or `remote/api/revoke`. History
exposes in-person room messages and that contact's whole Delta Chat
conversation, including the companion's replies; other contacts stay hidden.
Room captures are room-wide and labelled. The Diagnostics tab starts an opt-in
capture, lists it, transcribes to a `.txt` sidecar, streams the WAV with range
support, and removes both files.

**Commands and errors.** Each capability sits behind one router method in
`brain/src/router.ts`: `setVolume`/`setSleep`, `setVoice`, `setListening`,
`openStory`/`restartStory`, `startCapture`, and `sayOnCompanion`. The admin
API, agent tools, Delta Chat commands, the device menu, and the remote page
decode a typed request and only format the router's result, so validation and
side effects cannot drift between transports.

**Firmware.** ESP-IDF C for ESP-VoCat v1.2 and ESP32-S3-BOX-3. Renders
360×360 (VoCat) or 320×240 (BOX-3) RGB565 frames, draws a fallback face when
offline, reports telemetry, accepts OTA, and advertises `caps.bridge_endpoint`
in `hello` so the edge owns endpointing.

## Code layout

| Path | Contents |
| --- | --- |
| `brain/src/` | Node 26 TypeScript brain: HTTP, DB, router, Delta Chat, OpenCode, stories, library reader, timers, portraits, remote controls, CLI, cross-process E2E. |
| `lark/cmd/lark-edge/` | Edge entry point, OTA push command, deterministic fake models for the E2E test. |
| `lark/internal/edge/` | Terminal server, IPC, capture/utterance loop, TTS prefetch, WAV, models. |
| `lark/internal/audio/` | WAV/PCM synthesis and resampling, melody/SFX, Smart Turn, Parakeet STT, Kokoro TTS. |
| `lark/internal/pixel/` | Canvas, icons, screens, scene/menu layout, portrait normalization, LUT grades, frame encoder. |
| `lark/internal/proto/` | Generated v1 terminal wire codec shared by edge and firmware. |
| `lark/internal/library/` | FTS5 downloads, index builders, search/read, story-text import, disk planner, `library` CLI. |
| `lark/protospec/` | Single source of truth for the terminal wire protocol; generates C header, Go codec, docs, and fixtures. |
| `web/remote/` | Remote control page assets served by the brain. |
| `web/remote-ui/` | Headless-Chromium Playwright suite for that page. |

`scripts/lark` is the operator CLI: most commands call the brain's loopback
HTTP API; `ota` and `library --download|--build` route to `lark-edge`; `deploy`
runs `scripts/setup-host.sh`. Setup installs the compiled edge to
`~/.local/share/lark/lark-edge` and symlinks this wrapper next to it.

## Data model

`~/.local/share/lark/lark.db`, SQLite WAL. Private (`0700` directory, `0600`
file). `brain/src/db.ts` creates the same schema as the v1 Go server, byte for
byte, so an existing database opens with no migration. Breaking storage changes
are documented in the changelog and applied while services are stopped.

| Table | Purpose |
| --- | --- |
| `companions` | Name, setup state, personality, voice, form, portrait id, listening, Delta Chat account id. |
| `terminals` | Connected board id, kind, attached companion, last IP/port, last-seen time. |
| `terminal_prefs` | Per-terminal volume level. |
| `sessions` | Companion → OpenCode session id, contract hash, last binding write, context cutoff. |
| `messages` | Canonical chat history: user, companion, intercom, and cards, with source, channel, title, timestamp. |
| `memory` | Versioned key/value notes in `companion:<id>` or `shared` scopes. |
| `timers` | One-shot timers: label, speech, chime, wake-agent flag, fire time, notes. |
| `stories` | Twee 3 source, author, passage count. |
| `story_progress` | Per companion and story: passage, page, flags. |
| `meta` | Small server keys, including saved remote tunnels. |

On disk:

- `edge.sock` — Unix IPC socket between brain and edge.
- `portraits/` — four-pose PNG sheets plus derived pixel backgrounds.
- `library/` — `gutenberg.db`, `simple.db`, `enwiki.db`, `books/`, `raw/`
  download staging.
- `deltachat/` — one RPC account directory per companion.
- `opencode/` — embedded OpenCode config, sessions, cache, state, and imported
  provider credentials.
- `stt-models/`, `tts-models/`, `lib/`, `bin/` — pinned model and native assets.

## A voice turn

1. **Tap → mic ≈ 24 ms.** Firmware starts capture and acknowledges
   `rec_start` (measured 14.5–28.7 ms). Audio is 16 kHz mono PCM over UDP;
   the edge holds a 400 ms pre-roll for listening mode.
2. **Silence-driven endpointing in the edge.** Silero VAD must agree with an
   adaptive room floor before audio counts as speech. Silence is measured from
   the last gated voice frame, never from transcript growth. Commit after
   **400 ms** of gated silence when Smart Turn ≥ 0.90 and the transcript does
   not end in a function word, possessive, article, preposition, auxiliary or
   filler; otherwise at the **1.2 s** cap. A continuous monologue commits at
   **20 s**. Parakeet probes run every 250 ms; probe text only advances when the
   decoded audio contained gated speech. Speculation starts after 180 ms quiet
   when Smart Turn ≥ 0.40; a new word cancels a speculative call.
3. **STT.** Whole-clip Parakeet on Vulkan: p50 **96 ms**, p95 **133 ms** in the
   engine benchmark; live turns usually report 54–74 ms. The edge sends
   `turn.speculate`, `turn.commit`, and `turn.cancel` events.
4. **OpenCode in the brain.** A speculative or committed prompt goes to the
   companion's long-lived session. First token p50 ≈ **1.47 s**, p95 ≈
   **1.90 s** for the primary model.
5. **Kokoro TTS in the edge.** The brain streams reply sentences as
   `terminal.speak_chunk`; the edge synthesizes and queues PCM ahead of display
   frames. Cached openers and first-chunk splitting make the first words tens of
   milliseconds.
6. **Playback.** The device reports playout completion as `playback.done`, and
   the brain records the turn.

Screens are static: a frame is encoded only when state changes (or the Stats
HUD refreshes every 2 s). There is no animation stream competing with audio.

## Invariants and policies

- **No runtime compatibility shims.** The v1 → v2 cutover is zero-schema: the
  brain opens the existing database directly. Every other breaking database,
  config, protocol, or API change is a changelog entry applied by the
  `lark-upgrade` skill (`.opencode/skills/lark-upgrade/`) while services are
  stopped. Upgrades never carry old fields or fallback readers forward.
- **One owner per resource.** Only the edge touches terminals, models, and
  capture DSP. Only the brain touches SQLite, Delta Chat, OpenCode, and the
  HTTP API. They share nothing but the typed IPC socket.
- **One session per companion, cache-friendly prompts.** Context has four
  layers: the static agent prompt plus tool instructions; one session
  instruction entry `lark.companion` (`{id, name, personality}`) written at
  session create and refreshed when a name or personality edit lands, never per
  turn; the stored transcript; and one live `[state: … now: …]` line on the
  current user message. Stored turns keep their historical state rows for
  auditing, but the `lark.context` plugin removes them from the model request
  (keeping the current user message, dropping all during compaction), so the
  model sees exactly one state block. The session is never rotated for a new
  turn.
- **Audio and control beat frames.** Edge outbound ordering is control → TTS
  PCM → latest coalesced frame. Frames may be dropped; audio and control may
  not.
- **Listening.** Listening is one boolean setting per companion. A window
  re-arms after each reply, lasts at most **30 s**, and closes on a tap or
  silence. After **10 s** without a reply, a new turn must contain the
  companion's name (normalised edit distance ≤2, checked in the first five
  words); nameless speech is ignored as background chatter. Speech with the
  name but no usable transcript gets one short pre-cached "didn't catch that"
  line and re-arms.
- **Screens are static.** No animations, tickers, or continuous redraws; all
  layout is deterministic for both geometries.
- **Floristry layout.** One focal element per screen, quieter secondary
  chrome, 2/4/8 px spacing, 3 or 5 items per page where possible, keep round
  screens inside the safe square, and check every screen on both boards.
- **Pairing only through the device QR.** The brain pushes a Delta Chat QR card
  to the attached screen; there is no host-side pairing form.
- **Remote links are scoped and revocable.** A new `/remote` invalidates the
  previous token for that contact; `/remote off` and `revoke` stop the tunnel
  and drop every token.
- **Microphone captures stay local.** `lark capture` and the `/remote`
  Diagnostics tab share one listening-window path. WAVs and their transcripts
  live only in `~/.local/share/lark/debug/` (directory `0700`, files `0600`);
  they are never uploaded to a third party, and only the holder of a scoped
  link can play, transcribe, or delete them. They are never used as fixtures,
  one capture runs per companion at a time, and delete removes both files.
- **Library provisioning keeps 20% free.** The planner reserves 20 % of the
  volume and spends only 90 % of the remainder.
- **Retired protocol fields are tolerated.** The edge zeroes retired fields;
  firmware ignores unknown frame ops and accepts longer status payloads.
  Capabilities in `hello` decide which optional behaviours both sides use.

## Testing and QA

- `./scripts/check.sh` — ShellCheck, TypeScript typecheck and the whole brain
  suite (including the real Go-edge ↔ brain cross-process E2E), Python setup
  tests, Go vet and race tests, strict host C tests for both board layouts,
  tracked-file hygiene, and shim/error-suppression guards. Offline; no GPU or
  model weights.
- `--remote-ui` — headless-Chromium DOM tests for the remote page.
- `--fast [ref]` — build, vet, and only the areas related to the diff against
  `ref` (default: the last green full gate, else `origin/main`). Local inner
  loop only; CI runs the full gate.
- `lark doctor` prints one line per check and exits non-zero on failures.

See [README.md](README.md) for setup and API tables, [docs/testing.md](docs/testing.md)
for per-area checks, `--fast`, flags and measured timings,
[docs/endpointing.md](docs/endpointing.md) for endpoint policy, and
[docs/stt-speed.md](docs/stt-speed.md) for STT numbers.
