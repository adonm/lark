# Architecture

Lark is two long-lived processes on one Linux host. A compiled Go **edge** owns
terminals, audio, speech models, endpointing, and frame encoding. A Node 26
TypeScript **brain** owns SQLite, OpenCode, Delta Chat, HTTP, stories, timers,
portraits, and remote controls. They speak one typed NDJSON protocol over a
Unix socket, so either side can restart without dropping the other's hardware
session.

Numbers below were measured on the reference host (16 CPU cores, one Vulkan
GPU, NVMe). Treat small differences as noise.

## Process and code map

**`lark-edge` — real-time media.** Binds the terminal listener on TCP and UDP
`:8766`, the brain IPC socket (`$LARK_DATA_DIR/edge.sock`, `0600`), and loads
Parakeet STT, Silero VAD, Smart Turn, and Kokoro TTS in the background. It owns
the terminal protocol (binary frames in RGB565/RLE565/LZ4/JPEG, touch events,
control messages, 16 kHz mono s16le PCM), endpointing, capture DSP, playback
queueing, and frame encoding. STT needs a Vulkan GPU; Kokoro and Silero run
through sherpa-onnx and the checksummed libraries in `$ORIGIN/lib`.

On start the edge prints `lark-edge terminal=… udp=… brain=…`. Every brain
connection immediately receives `ready` (model states) plus one
`terminal.connected` snapshot per live board, so a brain restart never disturbs
a hardware TCP session. `/health` exposes the same model states.

**`lark-brain` — state and conversation.** Owns HTTP `:8765` (admin API, remote
page), the household SQLite database through `node:sqlite` in WAL mode, one
`deltachat-rpc-server` child per companion, timers, portraits, the library
reader/planner, story parsing, and the embedded OpenCode runtime from
`@opencode/sdk`.

The agent tools are `lark_state`, `lark_patch`, `lark_story`, `lark_library`,
`lark_emit` (speak a line on a terminal), and `lark_audio` (metronome, short
tune, nearby-song identification, stop).

**Firmware.** ESP-IDF C for ESP-VoCat v1.2 and ESP32-S3-BOX-3. It captures and
plays audio, renders frames, draws a fallback face when offline, reports
telemetry, accepts pushed OTA, and advertises `caps.edge_endpoint` in `hello`
so the edge owns endpointing.

| Path | Contents |
| --- | --- |
| `brain/src/` | HTTP, DB, router, Delta Chat, OpenCode, stories, library reader, timers, portraits, remote controls, CLI, cross-process E2E. |
| `lark/cmd/lark-edge/` | Edge entry point and deterministic fake models for the E2E test. |
| `lark/cmd/lark-library/` | Standalone library provisioner. |
| `lark/cmd/lark-ops/` | OTA firmware build, upload, and confirmation. |
| `lark/internal/edge/` | Terminal server, IPC, capture/utterance loop, TTS prefetch, WAV, models. |
| `lark/internal/audio/` | WAV/PCM synthesis and resampling, metronome/tune synth, Smart Turn, Parakeet STT, Kokoro TTS. |
| `lark/internal/pixel/` | Canvas, icons, screens, scene/menu layout, portrait normalization, LUT grades, frame encoder. |
| `lark/internal/proto/` | Generated terminal wire codec shared by edge and firmware. |
| `lark/internal/library/` | FTS5 downloads, index builders, story-text import, and the `lark-library` CLI. |
| `lark/protospec/` | Single source of truth for the terminal wire protocol; generates the C header, Go codec, docs, and fixtures. |
| `web/remote/` | Remote control page assets served by the brain. |
| `web/remote-ui/` | Headless-Chromium Playwright suite for that page. |

`scripts/lark` is the operator CLI: most commands call the brain's loopback HTTP
API; `ota` routes to `lark-ops`, `library --download|--build` routes to
`lark-library`, and `deploy` runs `scripts/setup-host.sh`. Setup installs all
three binaries to `~/.local/share/lark/` and symlinks this wrapper next to them.

## Voice turn

![Voice-turn timeline: mic RTP, edge endpointing, Parakeet STT, OpenCode first token, streamed Kokoro TTS, playback](docs/diagrams/voice-turn.svg)

1. **Tap → mic.** Firmware starts capture and acknowledges `rec_start`
   (measured 14.5–28.7 ms). Audio is 16 kHz mono PCM in 20 ms RTP packets; the
   edge holds a 400 ms pre-roll for listening mode.
2. **Endpointing in the edge.** Silero VAD must agree with an adaptive room
   floor before a frame counts as speech. Silence is measured from the last
   gated voice frame, never from transcript growth. The edge commits after
   **400 ms** of gated silence when Smart Turn ≥ 0.90 and the transcript does not
   end in a function word, possessive, article, preposition, auxiliary, or
   filler; otherwise at the **1.2 s** cap, extended to **2.2 s** for short
   hesitant fragments. A continuous monologue commits at **20 s**. Parakeet
   probes run every **250 ms**. Speculation starts after **180 ms** of quiet when
   Smart Turn ≥ 0.40; new voiced audio or a changed transcript cancels it. Full
   policy: [docs/endpointing.md](docs/endpointing.md).
3. **STT.** Whole-clip Parakeet on Vulkan with 150 ms of silence padding on each
   side: p50 **96 ms**, p95 **133 ms** in the engine benchmark. The edge sends
   `turn.speculate`, `turn.commit`, and `turn.cancel` events.
4. **Model turn.** A speculative or committed prompt goes to the companion's
   long-lived OpenCode session. First token p50 ≈ **1.47 s**, p95 ≈ **1.90 s**
   for the primary model. A primary that accepts a prompt but emits nothing for
   **8 s** is interrupted and retried once on the fallback.
5. **Speech.** The brain splits the streaming reply into sentences and sends
   `terminal.speak_chunk`; the edge synthesizes and queues PCM ahead of display
   frames. Playback prefill is **250 ms**.
6. **Playback.** The device reports `playback.done`; the brain records the turn.

Screens are static: a frame is encoded only when state changes. Nothing
animates on a continuous stream that could compete with audio.

## Wire protocol

![Wire transport: TCP control and frames, UDP RTP microphone uplink, and reconnect behaviour](docs/diagrams/wire.svg)

The terminal opens one TCP connection on `:8766` for `HELLO` or `RESUME`,
binary frames, control messages, telemetry, touch, and keepalive. The edge
replies with `WELCOME`, display frames, recording and speech control, status, and
`PONG`. Every message is an 8-byte little-endian envelope (magic `0x4C`, type,
seq, length) followed by the payload; payloads above 320 KiB are rejected.

Microphone audio travels separately as RTP over UDP. Each packet is one 20 ms
frame: 12-byte header, payload type 96, 320 samples, 640 bytes of 16 kHz mono
s16le, 652 bytes on the wire. The edge drops duplicates, reordering, unknown
SSRCs, and packets from a different source address.

A fresh `HELLO` takes over immediately; a live session closes as `replaced` and
the edge issues a new SSRC. `RESUME` is accepted for **30 s** after a disconnect
and keeps terminal state while swapping the UDP stream. The edge sends `PING`
every **12 s** awake and **30 s** asleep; a session read times out after **75 s**.
Firmware retries at **1 s**, backing off to **30 s**; `WELCOME` resets the delay.

The full message catalogue is generated from `lark/protospec/spec.go` into
[docs/protocol.md](docs/protocol.md). Do not edit that file by hand; change the
spec and run `cd lark && go generate ./...`.

The brain↔edge IPC is one JSON object per line over the Unix socket. Edge events
carry `type` (`ready`, `terminal.connected`, `terminal.telemetry`,
`turn.speculate`, `turn.commit`, `turn.cancel`, `turn.not_caught`,
`pointer.action`, `playback.done`, `capture.progress`, `capture.done`). Brain
commands carry `cmd` and an optional `id` for request/response RPC (`terminal.*`,
`stt.transcribe`, `portrait.normalize`, `capture.*`, `health`). A stalled brain
must not stall hardware timing, so events are dropped rather than blocking the
edge.

## Data model

![Data model: companions own terminals, sessions, messages, timers, and story progress; stories are shared](docs/diagrams/data-model.svg)

`~/.local/share/lark/lark.db`, SQLite WAL. Private (`0700` directory, `0600`
file). `LARK_DB_PATH` overrides it. `brain/src/db.ts` creates the schema.

| Table | Purpose |
| --- | --- |
| `companions` | Name, setup state, personality, voice, form, portrait id, listening, Delta Chat account id, identity sync state. |
| `terminals` | Connected board id, kind, attached companion, last IP/port, last-seen time. |
| `terminal_prefs` | Per-terminal volume level. |
| `sessions` | Companion → OpenCode session id, contract hash, last binding write, context cutoff. |
| `messages` | Canonical history: user and companion rows, with source, channel, sender, `peer`, card title, and timestamp. |
| `memory` | Versioned key/value notes in `companion:<id>` or `shared` scopes. |
| `timers` | One-shot timers: label, speech, chime, wake-agent flag, fire time, notes. |
| `stories` | Shared Twee 3 source, author, passage count. |
| `story_progress` | Per companion and story: passage, page, flags. |
| `meta` | Small server keys, including saved remote routes and token hashes. |

`messages.peer` is the normalized Delta Chat address for off-device rows and
empty for in-person rows. `/remote` history uses it to show only that contact's
conversation. Raw remote tokens are never stored; `meta` keeps their hashes.

On disk:

- `edge.sock` — Unix IPC socket between brain and edge.
- `portraits/` — four-pose PNG sheets plus derived pixel backgrounds.
- `library/` — `gutenberg.db`, `simple.db`, `enwiki.db`, `books/`, `raw/`
  download staging.
- `deltachat/` — one RPC account directory per companion.
- `opencode/` — embedded OpenCode config, sessions, cache, state, and imported
  provider credentials.
- `stt-models/`, `tts-models/`, `lib/`, `bin/` — pinned model and native assets.
- `debug/` — local microphone captures and transcripts.

## Agent and tools

The brain embeds `@opencode/sdk` in its own process. It keeps OpenCode's
configuration, session data, cache, and state under
`~/.local/share/lark/opencode/`, loads only the `lark.context`,
`lark.model-fallback`, and `lark.tools` plugins, and never reads personal
OpenCode sessions, plugins, or coding `AGENTS.md`. Setup copies the
`opencode` provider credential from the user's OpenCode SQLite store once;
providers authenticated later can be copied by rerunning
`scripts/setup-host.sh`.

| Role | Default model |
| --- | --- |
| Companion (`lark`) | `opencode/gemini-4-hydrogen#low` |
| Fallback | `opencode/gpt-6-luna#low` |
| Deep subagent (`deep`) | `opencode/gemini-4-hydrogen#high` |

An explicit `429`, `503`, or `403` before output or tools can retry once on the
fallback. The next turn starts on the primary. Stalls, interruptions, and
permission failures do not switch models.

| Tool | Purpose |
| --- | --- |
| `lark_state` | Read companions, terminals, messages, timers, memory, and stories. Select slices with `include`. |
| `lark_patch` | Update terminal volume, companion fields, memory, or a timer. Memory writes are version-checked and atomic. |
| `lark_story` | Create, update, read, or delete a validated Twee 3 story. |
| `lark_library` | Search, read, list, or import from the local text library. |
| `lark_emit` | Speak a line on a terminal. |
| `lark_audio` | Sample-exact metronome, short hum/chime/pluck tune, nearby-song identification, or stop. |

`lark_audio.identify` records a 7 s 16 kHz WAV, fingerprints it locally with the
pinned MIT `shazamio-core` WASM module, asks Shazam's public discovery endpoint
once, and deletes the capture in `finally`. No API key or extra binary is
needed; `lark doctor` fails if the recognizer cannot load.

Read persistent values as `memory[scope][key].value`; write them through
`lark_patch`'s `memory` argument. The agent checks `lark_library` first for
stories, books, and reference facts, and uses OpenCode's `websearch` only when
the library misses or the answer needs current information.

**One Voice Rule:** only assistant prose is spoken as the reply, not tool output
or reasoning. Each turn starts with a `[Speaker: …]` or `[Event: timer]` header
and ends with one `[state: …]` line. Companion identity is the session
instruction entry, written at session create and refreshed when the name or
personality changes. The agent uses `lark_state` for live context.

Context has four layers: the static agent prompt plus tool instructions; one
session instruction entry `lark.companion` (`{id, name, personality}`); the
stored transcript; and one live `[state: … now: …]` line on the current user
message. Stored turns keep their historical state rows for auditing, but the
`lark.context` plugin removes them from the model request, so the model sees
exactly one state block. The session is never rotated for a new turn.

## HTTP API

Authenticated requests require `X-Lark-Token` (`/health` is unauthenticated; the
remote page and its SSE stream use scoped tokens instead).

| Lark · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Edge model states, OpenCode, TTS, and Delta Chat readiness |
| `GET /api/library` | Library plan: installed packs, current build, pending or space-skipped packs, and free/budget bytes |
| `GET /api/diagnostics` | Live router state plus Delta Chat readiness and orphaned accounts |
| `GET /api/companions` | List companions (`id`, `name`, `voice`, `online`, `dormant`, `terminals[]`) |
| `POST /api/companions` | Create a companion (`{"name":"Mint","form":"a fox"}`) |
| `GET /api/companions/{id}`, `PATCH /api/companions/{id}`, `PUT /api/companions/{id}`, `DELETE /api/companions/{id}` | Read, update, or delete a companion |
| `GET /api/terminals` | List terminals |
| `GET /api/terminals/{id}`, `PATCH /api/terminals/{id}`, `DELETE /api/terminals/{id}` | Read, update volume/attachment, or delete a terminal |
| `POST /api/terminals/{id}/attach`, `POST /api/terminals/{id}/detach` | Attach to `{"companion_id":"…"}` or detach |
| `POST /api/terminals/{id}/volume` | `{"volume":55}` |
| `GET /api/terminals/{id}/hits` | Live screen state, revision, and touch hit targets |
| `POST /api/capture` | `{"companion_id":"cake","seconds":20}` → local 16 kHz mono WAV under `~/.local/share/lark/debug/` |
| `GET /api/remote/tunnels`, `DELETE /api/remote/tunnels/{companion}` | List or stop saved remote tunnels (no tokens exposed) |
| `GET /remote?token=…` | Scoped control page (also served through the companion's OpenTunnel) |
| `GET /remote/events?token=…` | Server-Sent Events stream for live state and story progress |
| `POST /remote/api/say`, `listen`, `volume`, `voice`, `story/open`, `story/restart`, `revoke` | One scoped JSON command per route |
| `GET /remote/api/history?cursor=…` | Scoped history page with channel, search, and cursor parameters |
| `POST /remote/api/capture` | `{"seconds":15}` → opt-in capture on the companion's voice owner; `409` while one runs |
| `GET /remote/api/captures` | Scoped list with size, duration, transcript, and computing flag |
| `POST /remote/api/captures/{name}/transcribe`, `GET /remote/api/captures/{name}.wav`, `DELETE /remote/api/captures/{name}` | Transcribe to a `.txt` sidecar, play with range support, or delete both files |
| `GET /ota/…` | Firmware files published under `$LARK_DATA_DIR/ota/` |

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `connected` |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota/upload` | Upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Security and privacy

![Pairing, text-only Delta Chat, and scoped /remote link](docs/diagrams/remote-and-messaging.svg)

- **Pairing only through the device QR.** The brain pushes a Delta Chat QR card
  to the attached screen; there is no host-side pairing form. Only verified
  Delta Chat contacts reach a companion.
- **Delta Chat is text-only.** Incoming voice notes are decoded with `ffmpeg`
  and transcribed, then discarded. Replies and announcements are sent as text;
  Lark never creates an outbound audio attachment.
- **Remote links are scoped and revocable.** A token is bound to one companion
  and one Delta Chat contact. Each `/remote` request mints a fresh token and
  invalidates the previous one for that contact. `/remote off` or
  `remote/api/revoke` stops the tunnel and drops every token for the companion.
  Tunnels start lazily and are remembered by key, not raw token.
- **Microphone captures stay local.** `lark capture` and the `/remote`
  Diagnostics tab share one listening-window path. WAVs and transcripts live
  only in `~/.local/share/lark/debug/` (directory `0700`, files `0600`), are
  pruned after 14 days or 1 GiB, are never uploaded, and are never used as
  fixtures. One capture runs per companion at a time; delete removes both files.
- **OpenCode is isolated.** Lark never reads the user's personal OpenCode
  sessions, plugins, or coding `AGENTS.md`. Portrait generation and
  model calls use the `opencode` provider credentials or a Console API key in
  Lark's own data directory.
- **Transport is not encrypted.** Device TCP/UDP, the device HTTP server, and
  the admin API assume a trusted LAN. The device token gates `HELLO`; the admin
  token gates `/api/` and `/ota/`. `/remote` is exposed through OpenTunnel over
  HTTPS.

## Invariants and policies

- **No runtime compatibility shims.** Every breaking database, config,
  protocol, or API change is a changelog entry applied by the `lark-upgrade`
  skill while services are stopped. Upgrades never carry old fields or fallback
  readers forward. See
  [.opencode/skills/lark-upgrade/SKILL.md](.opencode/skills/lark-upgrade/SKILL.md).
- **One owner per resource.** Only the edge touches terminals, models, and
  capture DSP. Only the brain touches SQLite, Delta Chat, OpenCode, and the HTTP
  API. They share nothing but the typed IPC socket.
- **One session per companion, cache-friendly prompts.** The session is never
  rotated for a new turn; identity changes rewrite the one instruction entry.
- **Audio and control beat frames.** Edge outbound ordering is control → TTS PCM
  → latest coalesced frame. Frames may be dropped; audio and control may not.
- **Listening.** Listening is one boolean setting per companion. A window
  re-arms after each reply, lasts at most **30 s**, and closes on a tap or
  silence. After **10 s** without a reply, a new turn must contain the
  companion's name in the first five words (normalized edit distance ≤ 2);
  nameless speech is ignored as background chatter. Speech with the name but no
  usable transcript gets one short cached line and re-arms.
- **Screens are static.** No animations, tickers, or continuous redraws; all
  layout is deterministic for both geometries.
- **Floristry layout.** One focal element per screen, quieter secondary chrome,
  2/4/8 px spacing, 3 or 5 items per page where possible, round screens inside
  the safe square, and every screen checked on both boards.
- **OTA is push-only.** `lark-ops` uploads an image to the board over HTTP and
  confirms the running partition and firmware version after reconnect. Boards
  never pull.
- **Library provisioning keeps 20% free.** The planner reserves 20% of the
  volume and spends only 90% of the remainder.
- **Forward compatibility is capability-driven.** `hello` capabilities decide
  optional behavior; unknown frame ops are ignored and longer status payloads
  are accepted.

## Testing and QA

- `./scripts/check.sh` — ShellCheck, TypeScript typecheck and the whole brain
  suite (including the real Go-edge ↔ brain cross-process E2E), Python setup
  tests, Go vet and race tests, strict host C tests for both board layouts,
  diagram hygiene, tracked-file hygiene, and shim guards. Offline; no GPU or
  model weights.
- `--remote-ui` — headless-Chromium DOM tests for the remote page.
- `--fast [ref]` — build, vet, and only the areas related to the diff against
  `ref` (default: the last green full gate, else `origin/main`). Local inner
  loop only; CI runs the full gate.
- `lark doctor` prints one line per check and exits non-zero on failures.

See [docs/testing.md](docs/testing.md) for per-area checks, flags, and measured
timings.
