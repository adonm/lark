# Lark QA Report

## Current state

As of 2026-11-24 on `main`:

- `./scripts/check.sh` passes: shell checks, Node tests, Go vet, the race suite,
  the live-tag compile, strict host C tests for both boards, and tracked-file
  hygiene. The race suite has 259 parallel tests and 19 sequential tests.
- `--e2e-stub` passes the 11 non-hardware QA scenarios against one isolated
  stub server in about 9 s.
- `--remote-ui` passes the headless-Chromium page suite in about 3 s.
- `--qa` passes the sim suite; `--qa-hw` adds the hardware subtests and skips
  them when no board is attached.
- Static prompt: 4,155 bytes (agent 3,786 + MCP instructions 369) against the
  4,250-byte budget. A worst-case state line (3 terminals, 5 timers, 5 stories,
  60-byte labels) measures 1,176 bytes against its 1,280-byte bound.
- Companion models: primary `opencode/deepseek-v4.1-flash#low`, deep
  `opencode/glm-5.3#high`, fallback `opencode/glm-5.3-flash#low`.
- Firmware source is 0.21.0.

Open gaps:

- Every STT accuracy and latency number below uses synthetic Kokoro speech.
  Real child speech at room distance is still unmeasured; collect 2–3 minutes
  with `lark capture <companion> 60`, keep the WAVs local, and compare
  `lark stt-bench` output with what was said.
- Hardware QA, live Delta Chat, and tunnel checks need a board, a paired
  account, and `opentunnel`; they are not part of the offline gate.

## Historical runs

Commands, flags, model ids, firmware versions, and measurements below are
reported as run on their dates. Several are retired; treat them as evidence,
not instructions.

### Listening reliability, 2026-11-23 (historical)

Three failures drove the change: turns that never sent after speech, turns cut
mid-sentence, and speech silently dropped. All reproduced in live
`LARK_DEBUG_ENDPOINT=1` logs and synthetic RTP tests.

Root causes:

1. Commit was measured from the probe that last changed the transcript, so
   Parakeet probe churn on room noise held the quiet and cap timers open.
2. The cap ignored VAD and fired on transcript age; the function-word guard
   covered only conjunctions and fillers.
3. `finishEndpoint` closed `stop` then cancelled the capture context; a late
   worker exited without a result, so the turn saw an empty capture.
4. The gate was VAD-only plus a fixed 20 % relative energy floor, which missed
   quiet distant speech and admitted steady noise.

Fixes:

- Silence is measured from the last frame that was VAD-positive and above an
  adaptive floor. Quiet commit 400 ms, cap 1.2 s, monologue 20 s. Transcript
  text cannot extend any of them. Probe text only advances when the decoded
  audio contained gated speech.
- The floor is learned from one second of non-speech and tracks downward
  instantly, upward slowly.
- The closing-pause guard covers articles, possessives, prepositions,
  auxiliaries, and copulas as well as conjunctions and fillers.
- A committed endpoint is recorded before `stop` closes, and a late `rec_end`
  from the previous capture is ignored.
- A segment with at least 1 s of gated speech and no usable transcript speaks
  one cached "I didn't catch that." line and re-arms. After 10 s without a
  reply, a listening turn needs the companion's name (edit distance ≤2 in the
  first five words).
- Listening is one boolean, one 30 s window, one `/remote` toggle.

Turn bench, real model, 7 runs × 3 utterances (20 diagnostic turns; one
first-turn row was lost during startup):

| Metric | Before | After |
| --- | ---: | ---: |
| End-of-speech → commit p50 / p95 | 241–1,080 ms spread | 400.3 / 407.5 ms |
| E2E p50 / p95 | 1.35–2.36 s spread (p50 ≈ 1.83 s) | 1,192 / 1,895 ms |
| Speculation | not counted | 21 started, 21 hit, 0 abort, 638 ms average overlap |
| Commits while speech still voiced | observed | 0 in 20 |

STT bench on synthetic fixtures (adult and +20 % pitch-shifted kid voices,
pink-noise bed): Parakeet 55–77 ms; exact transcripts except one dropped final
period; the noise-only clip returned empty.

Verification: the full gate passed with new quiet-speech, noise-only,
transcript-churn, monologue, window-close, name-path, feedback, commit-vs-cancel,
capture-permission, diagnostics, and listening-migration tests. The sim suite
passed in 10.9 s and the hardware suite in 22.5 s, including
`box3-b324/AutoCommitNoisyMic` (speech plus ~350 RMS noise, no Send). Live
captures wrote `0700` directories and `0600` files, transcribed both injected
clips, ignored 5 s of noise between them, and a window on `vocat-33d8`
committed at 400 ms of silence.

### Server cleanup, 2026-11-23 (historical)

Firmware 0.20.0 shipped in the same rollout.

- CI used the deleted module path; it now uses `lark/go.mod` and `lark/go.sum`.
- Companion deletion now clears welcome and story message meta plus the Delta
  Chat address/account maps, and calls `dcService.forget`.
- History queues stop through a `quit` channel instead of closing channels
  under writers.
- Portrait/form changes invalidate the cached background; replaced portrait
  directories are deleted and the cache is LRU-bounded.
- Speculative turns abort unless promoted; pre-synthesis uses a cancellable
  context.
- Listen captures no longer drop speech after 60 s of a longer window.
- `/remote` patches keyed rows in place instead of rebuilding the panel on each
  SSE push; the manifest uses token-free URLs.
- Deleted unrouted API types and dead WebSocket-era paths: tier 2 was +240/−752
  source lines, and `/remote` dropped the bundled Oat files in favour of eight
  theme variables.

Performance on the reference host (`-benchmem`, median of 3):

| Benchmark | Before | After |
| --- | --- | --- |
| `RenderPixelHome/vocat` | 352,217–359,354 ns, 590,202 B, 7 allocs | 290,180–290,610 ns, 370 B, 2 allocs |
| `RenderPixelHome/box3` | 307,912–312,405 ns, 393,592 B, 7 allocs | 259,200–261,066 ns, 225–310 B, 2 allocs |
| `RenderPixelMenu/vocat` | 320,542 ns, 591,549 B, 11 allocs | 317,431 ns, 591,554 B, 10 allocs |
| `FrameEncoderRLE` | 207,916 ns, 1,152 B, 1 alloc | 204,922 ns, 1,152 B, 1 alloc |

Endpoint rings shrank from 256 × 8 KiB to 256 × 640 B and are pooled. Probes
stop after the quiet window or near the cap, commit cancels pending probes, and
the STT window is 4 s. Screens and canvases are pooled after RGB565 conversion.
TTS inserts one silence chunk between sentences; the clip cache is 24 MiB. TCP
writes use `net.Buffers`.

The gate, live-tag compile, sim suite, remote-page suite (2.5 s), and README
route guards passed. Setup ran after rebasing on firmware 0.20.0; `lark status`
was healthy with all three boards online. One `lark sim` turn on `greg`
completed listening → thinking → speaking → done with first TTS 2.25 s after
speech.

### Sleeping-terminal ping backoff, 2026-11-23 (historical)

The server pinged every 12 s and woke a sleeping radio. The interval is now
30 s while the terminal reports sleeping and returns to 12 s when awake; the
75 s TCP read deadline is unchanged. `TestPingIntervalFollowsSleep` asserts
12 s → 30 s → 12 s.

### Device replies to Delta Chat read-outs, 2026-11-23 (historical)

Symptom: answering a read-out by voice produced a "couldn't reach" reply
instead of delivering to the sender. Three causes: the card Reply button
addressed the card title and never used Delta Chat; tap-to-talk dropped the
reply context; `emit.intercom` matching was narrower than `me.contacts`.

The fix arms a `turnReply` for 2 minutes, consumed by one tap-to-talk or
listening turn; the final reply goes back to the chat unless `emit` ran.
`[silent]` and empty replies send nothing. The card Reply target is the
conversation partner, and a clip for an offline peer goes by voice note.
`emit.intercom` matches display name, address-book name, address fallback, or
peer id exactly.

Tests: `TestDeviceReplyToDeltaChatHuman`, `TestDeviceReplyToDeltaChatCompanion`,
`TestDeviceReplyExplicitEmitWins`, `TestDeviceReplySilentSendsNothing`,
`TestDeviceTurnWithoutReadoutNotForwarded`,
`TestSayReadoutCardRepliesToHumanSender`,
`TestIntercomCardReplyTargetsConversationPartner`,
`TestEmitIntercomMatchesContactAddress`. Live `TestQADeltaChatDeviceReplyLive`
passed in 25.1 s; one earlier attempt timed out on the relay welcome.

### Remote controls, REST and SSE, 2026-11-23 (historical)

- A verified contact sent `/remote` and got one bare tunnel link. REST commands
  set listening, changed voice, and returned history. A second `/remote`
  invalidated the first token (401) and issued a replacement. `/remote off`
  made the token 401 on the main port and stopped the scoped listener.
- Through the real tunnel, `POST /remote/api/listen` measured p50 106 ms;
  the older chatmail `/say` round trip measured p50 2.63 s.
- `TestQADeltaChatLive` passed in 102.1 s; the throwaway companion and account
  were removed.
- The gate passed in 48.5 s. The remote-page suite passed 7/7 in 8.8 s.
- `TestRemotePagePWAAndScopedAuth`, `TestRemoteRESTCommandsAndHistoryCursor`,
  `TestRemoteEventsStreamsInitialStateAndChanges`,
  `TestRemoteTunnelCommandIssueRotateAndRevoke`,
  `TestRemoteTunnelRestoresSavedRouteAndToken`, and
  `TestRemoteMissingOpentunnelAndCompanionDelete` cover scoping, assets, SSE,
  rotation, restore, the missing `opentunnel` error, and teardown.
- The webxdc panel, its status-update handler, versioning, story player, and
  the old `/panel` command were deleted. Old panel messages are inert. `/story`
  sends a plain `.twee` attachment.

### Local library fast path, 2026-11-23 (historical)

The library is a latency feature: it answers reference, book, and story
questions in milliseconds so the agent does not fetch and summarise a web page.
`websearch` remains the path for current information and misses.

| Path | p50 | p95 | Notes |
| --- | --- | --- | --- |
| `library.search` in-process | 2.7 ms | 3.8 ms | 60 warm runs, 285,974-page Simple English index |
| `library.read` in-process | 0.02 ms | 0.02 ms | 10 runs, bounded 3.2 k characters |
| `library.search` over MCP/JSON | 17.2 ms | 18.1 ms | 20 runs, includes HTTP, JSON, and scheduling |
| `websearch` fetch | 786–962 ms | — | 3 live questions; median 851 ms, excludes summarisation |

With Wikipedia and Gutenberg downloading at the same time, in-process search
rose to p50 7.9 ms / p95 14.6 ms.

`TestQALocalLibraryBehaviour` (real OpenCode, fresh companions): baseline "Say
hello" first TTS 4,687 ms; "Why is the sky blue?" 2,885 ms; the three little
pigs story 3,395 ms; a Bitcoin price question tried the library, missed, and
fell back to web search at 4,136 ms.

Content at the time: Simple English Wikipedia 285,974 pages / 538 MB FTS5;
Project Gutenberg 2,994 of 3,000 pinned titles / 1.07 GB text; full English
Wikipedia downloaded at ~21 MB/s and indexed in the background, appearing
atomically.

### Silent story opens, 2026-11-23 (historical)

Menu, Memories → Reading, and remote story opens render the passage without
TTS; tapped choices stay silent. `TestOpenStoryFromMenuIsSilent` and
`TestConversationalStoryOpenStillNarrates` cover the silent and narrated paths;
voice choices still narrate the next passage.

### OpenCode Zen gateway, 2026-11-23 (historical)

- Models: `lark` = `opencode/deepseek-v4.1-flash#low`, `deep` =
  `opencode/glm-5.3#high`, fallback = `opencode/glm-5.3-flash#low`. The
  `opencode` provider auth was already imported in the dedicated workspace.
- Benchmark, 20 interleaved warm chat turns: `opencode-go/…` step p50/p95
  1,087/1,448 ms; `opencode/…` 1,058/1,523 ms. Gateway latency varies by
  ±300 ms between runs; the switch keeps one provider for all three models.
- Five hard prompts passed a fresh-session rubric: timer, story, intercom to
  Mint, a Rayleigh-style reasoning answer, and deep delegation.
- `TestTurnBench` confirmed the primary model on all 9 prompts.

### Webxdc panel v4, 2026-11-23 (historical)

The panel was later deleted. It kept the header design but was branded "Lark
Control" and two bugs made it unusable: history vanished and voice selection
never updated.

Root causes: the panel re-applied its own status updates as commands, so a
history response became a new request; one live message had accumulated
708,630 history updates. The flood buried the post-voice state.

Fixes: skip outgoing payloads and allow only known command ops; tag each
history request with a per-instance `sid` and monotonic `seq`; carry the voice
in `ack` and apply it immediately. `manifest.toml` and `icon.png` are generated
per companion, so a rename re-sends the panel.

The retired panel DOM suite passed 8/8; the gate passed in 53.3 s, the sim
suite in 10.9 s, and the live Delta Chat suite in 55.2 s. The deployed `greg`
chat had the old 708k-update message deleted for everyone.

### Latency measurement, 2026-11-23 (historical)

`lark sim` reported `speech_end_to_first_tts_ms` ~2.5–3.1 s while the bench
reported ~1.0–1.3 s because they measure different intervals: the sim starts at
end of speech audio before the Silero tail and includes cold-session
`llm_first_ms`; the bench starts at commit on a warm session.

- Sim: `stt_ms` 54–74 ms, `speech_end_to_commit_ms` 1,257–1,299 ms,
  `llm_first_ms` 905–2,113 ms, `e2e_ms` p50 1,293 / p95 1,864 ms.
- Hardware: `rec_start_us` 14.5–28.7 ms, `tts_first_us` 16–84 µs, `e2e_ms`
  p50 1,342 ms (`vocat-33d8`) / 1,124 ms (`box3-b324`), no audio drops.

### Scenario matrix, 2026-11-23 (historical)

Thirteen scenarios covered voice turns, interruptions, listening, menus,
stories, memories/voice/volume, timers, Delta Chat, intercom, multi-terminal,
look changes, recovery, and admin operations, on sim and hardware where
available. Pass rate was 100 % on the run recorded then; e2e p50/p95 was
1,293/1,864 ms on sim and 1,250/1,982 ms on hardware. The suite found ten bugs
in one pass, listed below.

### Bugs found, 2026-11-23 (historical)

1. `statusDone` emitted while TTS was still pending; now waits for pending TTS.
2. `leaveScene` left a stale companion status and `sim tap-label` raced the
   frame; status now resets and `/hits` carries a revision.
3. An omitted `companion_id` returned "Invalid companion ID"; the error now
   says to pass the ID from the turn header.
4. Intercom menu paging clamped to six tiles instead of the geometry page size.
5. List screens registered nav hits without drawing the nav bar.
6. `openStory` skipped the Start passage when no progress existed.
7. A Stop tap could be dropped during a speaking animation; `actionRev` spans
   unchanged hit layouts.
8. `dc.sendIntercom` ignored a renamed companion; it now matches
   `companion.Name`.
9. Timer companion fallback and resume frame order were restored and the test
   made order-independent.
10. The repeatable `lark/qa/*.sim` suite and `--qa`/`--qa-hw` were added.

### 30-minute soak, 2026-11-23 (historical)

29 voice turns across `vocat-33d8`, `box3-b324`, and `vocat-2cac` over
30.01 min, with 0 errors and 0 disconnects. Free heap moved by at most
−5,584 B per board, PSRAM did not move, and `audio_drops` stayed 0. Server RSS
grew +179.2 MB over the run (fixed below).

The RSS growth was glibc arena fragmentation across the ONNX Runtime threads,
Parakeet, and Silero, not a Go leak. `MALLOC_ARENA_MAX=2` plus
`mallopt(M_ARENA_MAX, 2)` plateaued RSS at 737 MB after 10 turns and 745 MB
after 100 in a 100-turn sim soak, down from 922 MB at turn 30 and 968 MB at
turn 60.

### Model and endpoint tuning, 2026-11-23 (historical)

- Static prompt and MCP instructions were trimmed from 5,039 to 4,444 cached
  tokens. Speculation started at 400 ms quiet, then moved to 180 ms quiet as
  the commit threshold and Smart Turn checks were tuned.
- Chat `e2e_ms` after tuning: p50 1,228 / p95 1,804 ms; timer 931/1,201 ms;
  reasoning 1,208/1,728 ms. Abort rate was 0 across 27 bench turns and the QA
  runs.
- DeepSeek TTFT on the dedicated server measured step p50 1,467 / p95
  1,900 ms, first text delta p50 1,485 / p95 1,892 ms, 38–160 uncached input
  tokens per warm turn, and 0–26 reasoning tokens.

### Companion visibility and reply routing, 2026-11-23 (historical)

- `turnPromptWithState` filtered on the wrong terminal key, so every prompt
  said `terminals: none`. It now lists only attached online hardware rows with
  id, board, battery, sleeping, and volume when present.
- A read-out context could hijack a later turn. The notice-spoken time is
  recorded, only a capture started after it can answer, the context clears on
  an earlier capture, and the TTL is 60 s.
- `patch` now defaults `companion_id` to the companion with a turn in flight
  and `terminal_id` to its voice owner or single online terminal.
- Story and listening prompts were corrected; the state line gained `form:`
  and `listen: on`/`off`.
- After the visibility fix, a live cake turn reported
  `vocat-33d8(esp-vocat,online,battery:96,vol:90)` and answered "Ninety-six
  percent", matching the board.

### CI sharding, 2026-11-23 (historical)

CI kept one offline job measured at 187 s before the parallel suite. A
three-way race split was tried and rejected: its best case saved ~87 s
(187 s → ~100 s), under the 90 s bar, and every shard had to reinstall ffmpeg
for tests that depend on it. That install took 45 s to over 5 minutes on the
package mirror; one shard ran 402 s, slower than the unsplit job. The suite
parallelises inside one job instead, and Kokoro is skipped there when
`TTS_MODEL_DIR` has no model pack.

### Security, privacy, and library hardening, 2026-11-24

Fixed on `fix-security` (rebased onto `main` at `bf5bfbb`):

- Entropy failures fail the request instead of minting all-zero `/remote`
  tokens, portrait ids, or timer ids. `/mcp` now requires a loopback peer like
  `/debug/pprof`; the HTTP listener stays LAN-reachable for firmware OTA.
- Tunnel issue/stop/revoke no longer hold the manager lock across opentunnel
  CLI calls, persist the replacement token before retiring the old one, and
  report `deleteMeta` failures so a revoked token cannot reappear.
- A scoped remote token reveals in-person notes plus only the Delta Chat
  messages its own contact sent; companion replies and intercom stay private.
  Its cookie is always `Secure`.
- Debug captures prune to two weeks / 1 GiB on start and save, list at most
  200, read transcripts and transcribe through bounds, and are removed with
  their companion; story backgrounds are removed with their story. `doctor`
  reports capture size.
- Identity edits are versioned and reconciled on the next turn; a failed PUT
  stays dirty and a newer edit always wins. A new session advances
  `context_after_ms` and deletes the replaced OpenCode session.
- Missing prompt, retired stored voice, broken library packs, orphaned Delta
  Chat accounts, and schema drift are surfaced by `doctor` instead of failing
  later as a generic 502.
- Gutenberg pins resolve through `repoRoot()`, pack builds share one lock (and
  a provision lock), and lock-open errors read as locked.

Measured (`go test -bench`, 24 stories x 12 passages): `listStories` 56.5 us
vs `listStorySummaries` 21.2 us; `remoteSnapshot` 101 us cold vs 71 us with
the compiled-progress cache. The remote page now loads pinned Oat 1.1.0 from
jsDelivr; `--remote-ui` stubs the CDN and passes offline.

Gates: `./scripts/check.sh`, `--e2e-stub`, `--qa`, `--remote-ui` all pass. Not
deployed; the endpoint workstream deploys last.
