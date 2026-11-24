# Architecture

A map of Lark for a new contributor or an OpenCode agent working in this repo.
Every number here was measured on the reference host (16-core CPU, one Vulkan
GPU, NVMe) on 2026-11-23; treat small differences as noise.

## Components

**`lark serve` — one Go binary.** Owns the HTTP admin API, MCP endpoint, OTA
files, terminal hub, SQLite database, Parakeet STT, Silero VAD, Smart Turn,
Kokoro TTS, Delta Chat bridge, timers, library planner, and remote-control
tunnels. `lark serve` needs a Vulkan GPU for STT; Kokoro and Silero run in
process through sherpa-onnx and the libraries in `$ORIGIN/lib`.

Startup binds HTTP `:8765` and the device listener `:8766` first, then loads
Parakeet, Silero, Smart Turn and Kokoro in the background. `/health` reports
each model as `loading`, `ready` or `error`, and `lark status` prints the same
states. A turn that needs a model still loading waits for it (bounded by the
turn deadline) instead of failing, so the first turn after a restart is safe.
`lark deploy` builds, installs atomically, refreshes the dedicated OpenCode
config, restarts and waits for readiness.

**Terminal protocol — TCP and UDP `:8766`.** Boards connect to the native
protocol (not HTTP): binary frames in RGB565/RLE565/LZ4/JPEG, touch events,
control messages, and 16 kHz mono s16le PCM. The TCP client carries control and
TTS audio with explicit priority (control, then PCM, then the latest coalesced
frame); UDP carries RTP PCM from the mic. HTTP `:8765` serves admin, MCP, OTA,
and remote pages.

**OpenCode — loopback `:49375`.** A dedicated `lark-opencode.service` with its
own config, sessions, cache, and workspace. One long-lived session per
companion. Lark talks to it over SSE. The agent gets the `tools.lark` MCP tools:

| Tool | Purpose |
| --- | --- |
| `state` | Read keyed companions, terminals, timers, contacts, and scoped JSON. |
| `patch` | Update terminal volume/sleep, companion name/form/voice/portrait/listening, memory, timers. |
| `story` | Create, update, read, or delete a validated Twee 3 story. |
| `library` | Search/read/list/import from the local text library. |
| `emit` | Intercom, speak on a terminal, play notes or a named sound. |

A `deep` subagent (`opencode/glm-5.3#high` by default) handles multi-step
plans, hard questions, and stories. The primary companion model is
`opencode/deepseek-v4.1-flash#low`; the fallback router retries once on
`opencode/glm-5.3-flash#low` for a rate limit or supported upstream rejection.

**Delta Chat.** One `deltachat-rpc-server` child process and one Chatmail
account per companion. Contacts pair by scanning the companion's QR on the
device screen. Commands: `/say`, `/private`, `/help`, `/story`, `/stories`,
`/remote` (and `/remote off`). Every companion has its own address; Lark keeps
verified contacts and routes intercom through the Chatmail relay.

**Library.** Three SQLite FTS5 packs provisioned in the background, pending
packs smallest first: Simple English Wikipedia, Project Gutenberg, then English
Wikipedia when the volume can hold the peak download plus index. Search returns
in tens of milliseconds, so the agent checks it before `websearch`.

**`/remote`.** A phone-first control page served at `/remote?token=…` and
through an OpenTunnel subdomain. It ships no framework bundle: Oat's theme
variables and local CSS style the page. REST commands live under
`/remote/api/…`; live state is one Server-Sent Events stream at
`/remote/events`. A token is scoped to one companion and one contact, minted
per request, and revoked by `/remote off` or `remote/api/revoke`. History
exposes in-person room messages and that contact's own Delta Chat messages;
room captures are room-wide and labelled. The Diagnostics tab starts an opt-in
capture with `POST /remote/api/capture`, lists it at `GET
/remote/api/captures`, transcribes to a `.txt` sidecar with `POST
/remote/api/captures/{name}/transcribe`, streams the WAV with range support at
`GET /remote/api/captures/{name}.wav`, and removes both files with `DELETE
/remote/api/captures/{name}`. Capture progress arrives as `capture` SSE events.

**Commands and errors.** Each capability sits behind one command in
`commands.go`: `setVolume`/`setSleep`, `setVoice`, `setListening`,
`openStory`/`restartStory`, `startCapture`, and `sayOnCompanion` (say). The
admin API, MCP tools, Delta Chat commands, the device menu, and the remote page
decode a typed request (`requests.go`, unknown fields rejected with a field
error) and only format the command's result, so validation and side effects
cannot drift between transports. Commands return `problem` or `fieldError`;
each transport renders that one error in its own status or text shape.

**Firmware.** ESP-IDF C for ESP-VoCat v1.2 and ESP32-S3-BOX-3. Renders
360×360 (VoCat) or 320×240 (BOX-3) RGB565 frames, draws a fallback face when
offline, reports telemetry, accepts OTA, and drives endpointing when it
advertises `caps.bridge_endpoint` in `hello`.

## Data model

`~/.local/share/lark/lark.db`, SQLite WAL with separate reader and writer
pools. Private (`0700` directory, `0600` file). `initSchema` creates the
baseline and applies the guarded column renames/drops listed in the changelog;
no other migration runs at startup.

| Table | Purpose |
| --- | --- |
| `companions` | Name, setup state, personality/vibe, voice, form, portrait id, listening on/off, Delta Chat account id. |
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

- `portraits/` — four-pose PNG sheets plus derived pixel backgrounds.
- `library/` — `gutenberg.db`, `simple.db`, `enwiki.db`, `books/`, `raw/`
  download staging, `setup.log`.
- `deltachat/` — one RPC account directory per companion.
- `backups/lark-YYYY-MM-DD.db` — daily `VACUUM INTO` snapshots, seven kept.
- `stt-models/`, `tts-models/`, `lib/`, `bin/` — pinned model and native assets.

## A voice turn

1. **Tap → mic ≈ 24 ms.** Firmware starts capture and acknowledges
   `rec_start` (measured 14.5–28.7 ms). Audio is 16 kHz mono PCM over UDP;
   the bridge holds a 400 ms pre-roll for listening mode.
2. **Silence-driven endpointing.** Silero VAD must agree with an adaptive
   room floor (learned for the first second and tracked during non-speech)
   before audio counts as speech. Silence is measured from the last gated
   voice frame, never from transcript growth. Commit after **400 ms** of
   gated silence when Smart Turn ≥ 0.90 and the transcript does not end in a
   function word, possessive, article, preposition, auxiliary or filler;
   otherwise at the **1.2 s** cap. A continuous monologue commits at **20 s**.
   Parakeet probes run every 250 ms, but probe text only advances when the
   decoded audio contained gated speech; unvoiced hallucinations are ignored.
   Speculation starts after 180 ms quiet when Smart Turn ≥ 0.40. A window
   with no gated speech discards at its span; tap-to-talk discards after 6 s
   without speech; capture limit 60 s. A new word cancels a speculative call.
3. **STT.** Whole-clip Parakeet on Vulkan: p50 **96 ms**, p95 **133 ms** in
   the engine benchmark; live turns usually report 54–74 ms.
4. **OpenCode.** A speculative or committed prompt goes to the companion's
   long-lived session. First token p50 ≈ **1.47 s**, p95 ≈ **1.90 s** for the
   primary model; prompt prep and submit add <10 ms.
5. **Kokoro TTS.** Sentences are synthesized as the reply streams. The first
   sentence is spoken as soon as its first chunk exists (cached openers and
   first-chunk splitting make this tens of milliseconds for common replies).
6. **Playback.** TTS PCM is queued ahead of display frames; the device
   reports playout completion and the turn is recorded in diagnostics.

Screens are static: a frame is encoded only when state changes (or the Stats
HUD refreshes every 2 s). There is no animation stream competing with audio.

## Invariants and policies

- **No runtime compatibility shims.** One documented exception:
  `initSchema` performs guarded, idempotent column renames and drops for a
  storage upgrade, probing first so a current database is left untouched.
  Every other breaking database, config, protocol, or API change is a
  changelog entry applied by the `lark-upgrade` skill
  (`.opencode/skills/lark-upgrade/`) while services are stopped. Upgrades
  never carry old fields or fallback readers forward.
- **One session per companion, cache-friendly prompts.** Context has four
  layers: the static agent prompt plus MCP `instructions` (tool and
  library/story policy); one session instruction entry `lark.companion`
  (`{id, name, personality}`) written at session create and refreshed when a
  name or personality edit lands, never per turn; the stored transcript; and
  one live `[state: … now: …]` line on the
  current user message. Stored turns keep their historical state rows for
  auditing, but the `lark.context` plugin removes them from the model request
  (keeping the current user message, dropping all during compaction), so the
  model sees exactly one state block. Prompt changes must keep the prompt
  budget test green; the session is never rotated for a new turn.
- **Audio and control beat frames.** Outbound terminal messages are ordered
  control → TTS PCM → latest coalesced frame. Frames may be dropped; audio and
  control may not.
- **Listening.** Listening is one boolean setting per companion. A window
  re-arms after each reply, lasts at most **30 s**, and closes on a tap or
  silence. After **10 s** without a reply, a new turn must contain the
  companion's name (normalised edit distance ≤2, checked in the first five
  words); nameless speech is ignored as background chatter. Speech with the
  name but no usable transcript gets one short pre-cached "didn't catch that"
  line and re-arms. `/api/diagnostics` exposes per-window counters for gate
  drops, empty/filler transcripts, commits, silent replies, feedback, name
  ignores, and close reason, plus text length and duration of recent segments.
- **Screens are static.** No animations, tickers, or continuous redraws; all
  layout is deterministic for both geometries.
- **Floristry layout.** One focal element per screen, quieter secondary
  chrome, 2/4/8 px spacing, 3 or 5 items per page where possible, keep round
  screens inside the safe square, and check every screen on both boards.
- **Pairing only through the device QR.** `POST /api/terminals/{id}/pair`
  pushes a Delta Chat QR card to the attached screen; there is no host-side
  pairing form.
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
  volume and spends only 90 % of the remainder. Installed packs and builds in
  progress are never restarted.
- **Retired protocol fields are tolerated.** The server zeroes retired fields;
  firmware ignores unknown frame ops and accepts longer status payloads.
  Capabilities in `hello` decide which optional behaviours both sides use.

## Testing and QA

- `./scripts/check.sh` — ShellCheck, Node and Python tests, Go vet and race
  tests, strict host C tests for both board layouts, tracked-file hygiene, and
  shim/error-suppression guards. Offline; no GPU or model weights.
- `--e2e-stub` — one isolated stub server and the non-hardware QA matrix
  (~9 s); model output and TTS are stubbed, capture and turns are real.
- `./scripts/check.sh --live` — virtual companions and firmware clients with
  real Parakeet, Silero, Kokoro, and a deterministic model stub.
- `--qa` — repeatable `lark sim` scenarios from `lark/qa/*.sim`; `--qa-hw`
  adds the live hardware suite.
- `--remote-ui` — headless-Chromium DOM tests for the remote page.
- `--fast [ref]` — build, vet, and only the tests related to the diff against
  `ref` (default: the last green full gate, else `origin/main`). Local inner
  loop only; CI runs the full gate.
- `--e2e-real` — dedicated OpenCode with the configured models (spends quota).
- `--deployed` — read-only installed-service and OTA probes.
- `lark sim` drives a virtual board; `lark device` talks to a real one.
- `lark doctor` prints one line per check and exits non-zero on failures.

See [README.md](README.md) for setup and API tables, [docs/testing.md](docs/testing.md)
for per-area checks, `--fast`, flags and measured timings, [lark/turn-taking.md](lark/turn-taking.md)
for endpoint policy, [lark/stt-speed.md](lark/stt-speed.md) for STT numbers, and
[docs/qa-report.md](docs/qa-report.md) for the latest QA run.
