# Endpointing

Lark keeps the mic open through thinking pauses and uses local transcription
to decide when to send. **Send** ends recording now; **Cancel** discards it.
Clients without `caps.bridge_endpoint` keep their own VAD.

## Policy

`scripts/setup-host.sh` installs `silero_vad_v5.onnx` from the Sherpa ONNX
`asr-models` release at `$XDG_DATA_HOME/lark/stt-models/` (defaults to
`~/.local/share/lark/stt-models/`). Setup and server startup verify SHA-256
`6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396`.
The server does not download models or need an endpoint environment flag.
Install the model before upgrading the server binary.

Silero v5 runs on CPU with 512-sample windows, a 0.50 speech threshold,
120 ms minimum speech and a 320 ms silence tail. A frame only counts as
speech when VAD is positive **and** its RMS clears an adaptive room floor.
The floor is the 10th percentile of the first second of non-speech frames and
then tracks upward slowly (instant downward) during non-speech. Floor-relative
margins are small in RMS counts, not dB: live boards report speech near 20–25
RMS over a room floor of 1–4. VAD still owns the speech decision; the floor
only vetoes noise, including VAD false positives and its silence tail.

| Rule | Default | Behaviour |
| --- | ---: | --- |
| Probe pause | 250 ms | Decode up to the last 4 s of one growing clip |
| Quiet commit | 400 ms | Gated silence; Smart Turn ≥ 0.90 and no incomplete trailing word |
| Cap | 1,200 ms | Gated silence; commits regardless of transcript completeness |
| Monologue | 20 s | Continuous gated speech commits even without a pause |
| No-speech discard | 6 s tap / window span | A window with no gated speech closes without a turn |
| Listen window | 30 s | Re-arms after each reply; tap or this cap closes it |

Silence is measured from the last frame that was both VAD-positive and above
the floor. Transcript growth cannot extend it. Parakeet probes can hallucinate
words on room noise, so probe text only advances the transcript when the
decoded audio contained gated speech; a first nonempty decode still counts,
and later edits need two new tokens (a sliding window may replace text without
growing the count). Speculation starts after 180 ms of quiet when Smart Turn
is at least 0.40 and is cancelled by new voiced audio or a changed transcript.

The quiet-commit path refuses to cut a turn that ends in a conjunction,
article, possessive, preposition, auxiliary, copula, filler, or trailing comma:
`his, her, the, a, an, and, but, or, so, because, if, to, of, in, on, at, is,
was, are, um, uh…` all wait for the 1.2 s cap or the next word. A segment with
at least one second of gated speech but no usable transcript speaks one short
cached line ("I didn't catch that.", "Sorry, what was that?", "Say that
again?"), re-arms the window, and never creates an agent turn. A cough or a
door does not trigger it.

## Listening mode

Listening is a single boolean per companion (`patch.companion.listening`).
After a reply the voice-owner terminal opens a 30 s window. For 10 s after the
reply finishes, follow-ups need no name. After that a new turn must contain the
companion's name in the first five words; a normalised edit distance up to 2
tolerates STT near misses ("Kate" for Cake, "Minty" for Mint). Nameless speech
is dropped silently as background chatter. When the name is present but the
rest is empty or filler, the companion says "didn't catch that" and re-arms.
`[silent]` replies and ignored speech both re-arm through normal playback
completion.

`GET /api/diagnostics` reports `listening` counters: windows, segments with
speech, gate drops, empty and filler transcripts, commits, silent replies,
"didn't catch that", name-required ignores, close reason (silence, tap, cap),
and recent segments with text length, capture duration, and speech duration.
Set `LARK_DEBUG_ENDPOINT=1` to log every gate transition, probe, Smart Turn
check, and commit decision with energy, floor, VAD, silence, and word counts.

## Wire and follow-up behavior

- Firmware that advertises `caps.bridge_endpoint:true` in `hello` drives
  untargeted Talk through
  `{"op":"rec","action":"start","target":"","endpoint":"bridge"}` when
  negotiated. Targeted intercom recording keeps its existing path.
- Audio is `0x01` followed by 16 kHz mono s16le PCM between `rec_start`
  and `rec_end`. An automatic cut sends `rec stop`; extra PCM is accepted
  until `rec_end` or a 450 ms timeout. `cancelled:true` discards the capture.
  The server discards a capture with no gated speech after 6 s of audio and
  caps any capture at 60 s. The firmware watchdog sends at 8 s of silence or
  60 s and closes the mic on Cancel, server stop, or disconnect. The mic is
  never silently reopened.
- Diagnostics `speech_end_to_commit_ms` measure estimated acoustic end to
  the server's `rec stop` (or `rec_end` for client Send/watchdog), excluding
  acknowledgement wait and final transcription. No end is recorded when
  speech is still heard at commit. `stt_ms` remains separate.
- **Say more** holds unpublished audio while the mic is open. It queues
  one normal follow-up behind the active reply, without interrupting or
  replaying the original prompt. Explicit **Stop** discards the queued
  follow-up. Tests cover the hold, prompt order, and Stop.

### Why steering stays off

Two disposable OpenCode 2.3.2 sessions on 2026-11-21 used a Haiku-class model.
Both admitted a distinct `delivery:"steer"` input while the session was active
and completed successfully:

- After the first text delta (tools denied), the first reply finished,
  then the steered user input and its separate answer appeared in history.
  The assistant rows exposed no parent input ID.
- After `session.tool.called`, a disposable MCP marker recorded one
  invocation. The tool completed despite steering; the steered answer
  followed. Admission did not guard against a tool already starting.

The API did not establish safe merge/replace behavior at the audio/tool
boundary. Lark keeps the gated follow-up; it does not send `steer` or
auto-replay a prompt after publishing audio or running tools. No production
session received a probe.

## Synthetic replay

Forty generated Kokoro clips (Bella, Heart, Puck), no recordings of children
or other real speakers. Includes 17 uninterrupted short requests/questions,
15 paused turns (0.7–4 s gaps), three trailing-filler turns, and five
multi-clause stories. Paused turns cover clause breaks, lists, details added
after a plausible stop, names and numbers. The last audible 10 ms block is the
estimated true end. Percentiles are nearest-rank, exclude premature commits,
and include observed Vulkan decode time before a decision, not network or
device acknowledgement. Results are synthetic and are not a field benchmark.

| Replay path | Premature commits | End → commit p50 / p95 | Short turns p50 / p95 |
| --- | ---: | ---: | ---: |
| Board RMS (legacy) | 18/40 | 1,500 / 1,500 ms | 1,500 / 1,500 ms |
| Browser RMS (legacy) | 18/40 | 1,500 / 1,510 ms | 1,510 / 1,510 ms |
| Silero, no text (before) | 0/40 | 4,540 / 4,570 ms | 4,550 / 4,580 ms |
| Silero, 950 ms for all complete text (comparison) | 2/40 | 3,360 / 4,570 ms | 1,020 / 3,380 ms |
| Silero + tuned local decode (after) | 0/40 | 3,360 / 4,560 ms | 2,130 / 3,380 ms |

No test turns were missed. The comparison cut a drawing request before its
modifier and a list before its remaining items. The tuned policy keeps the
fastest tier narrow and spends 2.1 s on other apparently complete text.
Stories still wait up to the patient threshold. The tuned path used 72 pause
decodes across the 40 fixtures (one per detected pause ≥600 ms).

| Story fragments with immediate simulated re-arm | Board / browser RMS | Silero before | Silero after |
| --- | ---: | ---: | ---: |
| Purple frog | 5 / 5 | 1 | 1 |
| Giant bee | 4 / 4 | 1 | 1 |
| Space dog | 3 / 3 | 1 | 1 |
| Backyard | 4 / 4 | 1 | 1 |
| Telling cat | 3 / 3 | 1 | 1 |

Re-arm starts before the next remaining clause and is only for counting
fragments; Lark never automatically reopens the mic.

Run with the `stage` prefix from the [STT build](stt-speed.md#build-and-reproduction):

```sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
cd lark
LARK_REPLAY_SILERO=/path/to/silero_vad_v5.onnx \
LARK_REPLAY_KOKORO=/path/to/kokoro-multi-lang-v1_0 \
LARK_BAKEOFF_MODEL="$stage/models/parakeet-unified-en-0.6b-Q8_0.gguf" \
CGO_CFLAGS="-I$stage/native/include" CGO_LDFLAGS="-L$stage/native/lib" \
  "$GO_BIN" test -tags parakeet -count=1 -v -run '^TestEndpointReplay$'
```

The replay saves no waveforms or transcripts. See the [earlier nine-clip
experiment](stt-speed.md#speculative-endpointing) for historical numbers.
