# Lark QA Report

Date: 2026-11-22  
Target: `main` (`6f444c9`..`35ec120`), firmware `0.19.0` (`vocat-33d8`, `vocat-2cac`, `box3-b324`)

## Latency Investigation (`sim` vs bench)

`lark sim` reported `speech_end_to_first_tts_ms` ~2.5–3.1 s while `e2e_bench_live_test.go` reported ~1.0–1.3 s because they measure different intervals:
- `sim` starts at **end of speech audio** (before the 1.26–1.30 s Silero endpoint silence tail) and includes cold-session `llm_first_ms` (~1.5 s).
- `e2e_bench_live_test` (`e2e_ms`) starts at **turn commit** (after endpointing) on a warm session (`1.0–1.3 s`).
- Breakdown on `sim`: `tap_to_rec_ctrl_ms` 0 ms; `stt_ms` 54–74 ms; `speech_end_to_commit_ms` 1,257–1,299 ms; `llm_first_ms` 905–2,113 ms; `tts_first_ms` 0.04–585 ms; `e2e_ms` p50 1,293 ms / p95 1,864 ms.
- Breakdown on hardware (`/status` + `/api/diagnostics`): `rec_start_us` 14.5–28.7 ms; `tts_first_us` 16–84 µs; `e2e_ms` p50 1,342 ms (`vocat-33d8`) / 1,124 ms (`box3-b324`); `tts_rms` 3,516–3,881; `audio_drops` 0.

## Scenario Matrix

| Scenario | Scope | Pass Rate | p50 / p95 | Notes |
| --- | --- | --- | --- | --- |
| 1. Voice turn | Sim (7×) + HW (5× VoCat, 5× BOX-3) | 17/17 (100%) | Sim e2e 1,293 / 1,864 ms; HW e2e 1,250 / 1,982 ms | Chat, timer, volume, tool state, long answer; fixed premature `statusDone` (`0cbb22f`). |
| 2. Interruptions | Sim + HW | 100% | Stop→idle <400 ms | Stop during speaking, 3× Stop during thinking, tap after reply; fixed `leaveScene` status (`631113c`) & `actionRev` (`82b360e`). |
| 3. Listening mode | Sim + HW | 100% | Re-arm <1.5 s | Voice on/off, follow-up re-arm, filler dropped, `[silent]` silent, tap ends; fixed missing `companion_id` error (`c8a9a60`). |
| 4. Menus & screens | Sim (VoCat + BOX-3) + HW | 100% | 0–8 ms | Every page/item, Back everywhere, safe area verified visually; fixed missing nav bar (`824ab8f`) & intercom paging (`f41e8ea`). |
| 5. Stories | Sim + HW | 100% | — | Open, page, tap/voice choices, Memories→Reading, voice-created branching story; fixed `openStory` start passage (`326424d`). |
| 6. Memories, voice, volume | Sim | 100% | — | Menu + voice changes persisted; memory notes rendered in Memories. |
| 7. Timers | Sim | 100% | — | Set, list, cancel, fire with chime + speech; survived `lark.service` restart. |
| 8. Delta Chat | Live (`TestQADeltaChatLive`) | 100% | 44.37 s full suite | QR pair, welcome once, plain, `/say`, `/private`, `/help`, `/stories`, `/story`, `/panel` webxdc, opus voice note, `.twee` valid/invalid, unknown cmd, non-contact ignored, Unpair. |
| 9. Intercom | Sim (`qa1`→`qa2`) | 100% | ~10 s (DC relay) | Card + speech delivered; fixed renamed peer lookup in `dc.sendIntercom` (`a55cdc6`). |
| 10. Multi-terminal | Sim + Sim; integration test | 100% | — | Frames & `STATUS` to both, tap ownership transfer, disconnect fallback, resume window. |
| 11. Look changes | Sim (2 Nano Banana calls) | 100% | 16.9–39.4 s | Transparency rule rejected bad outline (#1), succeeded (#2), recolor kept cached style. |
| 12. Robustness & recovery | Sim + HW | 100% | — | `lark.service` stop/start (20 s offline screen), `lark-opencode.service` restart, `deltachat-rpc-server` `SIGKILL` supervisor restart, OTA reboot. |
| 13. Admin & ops | CLI + scripts | 100% | — | `lark status`, `companions`, `terminals`, `lark ota --no-build` idempotence on `vocat-33d8`, `setup-host.sh` idempotence, `./scripts/check.sh --deployed`. |

## Bugs Found & Fixed

1. **`0cbb22f` — Premature `statusDone` in `finishTurn`:** `finishTurn` emitted `statusDone` while async TTS synthesis was still pending (`d.stopAudio != nil`), causing `sim wait idle` to exit before audio played. Added `d.stopAudio == nil && d.replyAudio == nil` guard + `TestFinishTurnWaitsForPendingTTS`.
2. **`631113c` — `leaveScene` left stale companion status & `sim tap-label` race:** `leaveScene` (Stop/Home) did not reset `companionStatuses` to `statusIdle`; `sim tap-label` could send a tap before the rendered frame arrived. Reset status in `leaveScene`, added `rev` to `/api/terminals/{id}/hits`, and synced `sim tap-label` + `TestLeaveSceneResetsCompanionStatus`.
3. **`c8a9a60` — Opaque error when `companion_id` omitted in `mcpPatch`:** Returning `"Invalid companion ID"` caused the model to think the companion had no valid ID. Clarified error to `"companion_id: required; pass the companion ID from the turn header"` + `TestMCPControl`.
4. **`f41e8ea` — Intercom menu paging clamped to `tilesPerPage` (6):** `drawerIntercom` clamped `d.menuPage` with `tilesPerPage=6` instead of geometry `perPage` (2 round / 3 rect), trapping `Next` on page 0. Clamped by geometry + `TestIntercomMenuPagingRoundScreen`.
5. **`824ab8f` — Missing nav bar in `renderPixelListWithHits`:** `Talk to…`, `Memories`, and `Stats` registered `navBarHits` without calling `drawNavBar`, leaving `Back`/`Next` invisible. Added `drawNavBar` + `TestRenderPixelListDrawsNavBar`.
6. **`326424d` — `openStory` skipped `enterPassage(Start)` when no progress existed:** `storySession.passageIdx` defaulted to `0`, making `currentPassageName() != ""` and rendering an empty story box. Initialized `passageIdx: -1` + `TestOpenStoryEntersStartPassage`.
7. **`82b360e` — `Stop` tap dropped during speaking animation:** `acceptPointerRevision` required `*rev == d.nativeRev`, rejecting hardware `Stop` taps when an animation frame was in flight. Tracked `actionRev` across unchanged hit layouts + `TestAcceptPointerRevisionAllowsAnimationFrames`.
8. **`a55cdc6` — `dc.sendIntercom` ignored renamed companion names:** Matched only stale Delta Chat contact names, not `companion.Name`. Added `companionByID` name match + `TestDCIntercomMatchesRenamedPeer`.
9. **`cf9bf12` — `mcpPatch` `timer.companion_id` fallback & resume frame order:** Restored `timer.companion_id` fallback in `mcpPatch` and made `TestResumeInsideWindowKeepsVoiceOwnershipAndExpiredResnapshots` order-independent across `msgFrame` and `msgVoiceOwner`.
10. **`35ec120` — Repeatable QA suite:** Added `lark/qa/*.sim`, `lark/qa_live_test.go`, and `./scripts/check.sh --qa` / `--qa-hw`.

## 30-Minute Soak Results

29 voice turns across `vocat-33d8`, `box3-b324`, and `vocat-2cac` over 30.01 min (0 errors, 0 disconnects):

| Metric | Start (0.1 min) | End (30.0 min) | Delta |
| --- | --- | --- | --- |
| `vocat-33d8` `free_heap` / `min_free_heap` / `free_psram` | 121,699 / 54,812 / 14,843,132 | 121,711 / 53,156 / 14,843,132 | +12 / -1,656 / 0 B |
| `box3-b324` `free_heap` / `min_free_heap` / `free_psram` | 130,047 / 41,396 / 15,379,380 | 130,055 / 41,396 / 15,379,380 | +8 / 0 / 0 B |
| `vocat-2cac` `free_heap` / `min_free_heap` / `free_psram` | 121,643 / 56,320 / 14,843,132 | 121,635 / 50,736 / 14,843,132 | -8 / -5,584 / 0 B |
| Server RSS / FDs | 755.7 MB / 26 | 934.9 MB / 26 | +179.2 MB / 0 FDs |
| Device `audio_drops` | 0 / 0 / 0 | 0 / 0 / 0 | 0 |

## Open Issues & Keep/Cut Recommendations

- **Server RSS growth (+179 MB over 29 turns) — FIXED:** Profiling (`runtime.MemStats`, `/debug/pprof/heap`, and isolated native loops) showed Go `heap_inuse` stayed flat (10–21 MB) with 0 goroutine leaks; the growth was glibc per-thread malloc arena fragmentation across Kokoro's 8 ONNX Runtime threads, Parakeet, and Silero VAD. Capping arenas (`MALLOC_ARENA_MAX=2` in `lark.service` and `mallopt(M_ARENA_MAX, 2)` in `lark/malloc_linux.go`) plateaued RSS at **737.12 MB (turn 10) → 745.11 MB (turn 100)** in a 100-turn sim soak (down from 922 MB at turn 30 and 968 MB at turn 60).
- **Unwired HTTP routes vs README — FIXED:** Removed stale `/ws`, `/app`, `/api/companions/{id}/messages`, `/api/timers`, and other retired routes from `README.md`, and added `TestREADMERoutesRegistered` to fail if the README route table drifts from `larkServer`.
- **Phase C semantic end-of-turn (Smart Turn v3.2) — SHIPPED:** Integrated `smart-turn-v3.2-cpu.onnx` (8.68 MB int8, BSD-2-Clause) via `onnxruntime_go` against the bundled `libonnxruntime.so` (1.28.2) with a zero-alloc Go Whisper log-mel extractor (`~25 ms` warm inference, threshold `0.90` at 200 ms silence, fallback to tuned hangover). Also fixed `lark sim` to send `msgRecStart` ack on `recOpStart` so bridge endpointing runs in sim. `speech_end_to_commit_ms` dropped from **1,257–1,299 ms** to **368.0 ms p50 / 368.4 ms p95** in sim and **370.54 ms** on `greg` (`box3-b324` via `/debug/mic`).
- **Spoken turns never auto-ending in noisy rooms & speaker misidentification (`greg`) — FIXED:**
  - *Root cause 1 (endpointing):* Live room turns on `box3-b324` recorded `speech_end_to_commit_ms: null` because Silero VAD stayed `IsSpeech() == true` on room/mic noise (preventing `paused()` from firing and resetting Smart Turn / `patient` 4.5 s hangover on every flicker, leaving only the 60 s `endpointLimit`). Replaced the hangover table with **transcript-progress endpointing**: Silero VAD + relative speech energy (`20%` of utterance RMS) only gate 250 ms Parakeet probes; `lastWordAt` advances only when normalized non-filler words grow, committing at `400 ms` (`quiet`) when Smart Turn ≥ 0.90 (unless trailing comma/conjunction/filler) or unconditionally at `1.2 s` (`cap`), with a `6 s` no-speech discard. Measured `speech_end_to_commit_ms`: **344.23 ms** on `box3-b324` (`greg` via `/debug/mic` + noise), **354.92 ms** in `sim` (`p50 344.23 ms / p95 354.92 ms`), ~4 probes/s during active speech and 0 decodes during quiet listen windows.
  - *Root cause 2 (turn header):* `[In person at greg's device · greg (greg) · voice]` placed `greg (greg)` in the middle of the header, so the LLM addressed the in-person user as `"Greg"`. Updated `turnHeader` and `recentDialogue` seeding to `[You are <name> (<id>). Speaker: someone in person at your device (name unknown) · ...]` (keeping `[state: ...]` at the end for prompt caching), updated `agents/lark.md`, cleared stale `sessions` rows so companions rebind cleanly, and verified `greg` no longer calls the speaker `"Greg"`.
  - *Voice preview:* Pre-cached one sample per curated voice at startup (`b.clips`) and played it immediately on `voice-set`/`voice-select` without recording to `messages` or the agent session.
- **Keep:** `lark sim`, `lark device`, `/api/terminals/{id}/hits`, and `./scripts/check.sh --qa` — they caught 8 real bugs in one pass and run in ~11 s.
