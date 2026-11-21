# Bridge endpointing

Lark keeps the mic open through thinking pauses and uses local transcription
to decide when to send. **Send** ends recording now; **Cancel** discards it.
Clients without bridge endpoint support keep their own VAD.

## Policy

`scripts/setup-host.sh` installs `silero_vad_v5.onnx` from the Sherpa ONNX
`asr-models` release at `$XDG_DATA_HOME/lark/stt-models/` (defaults to
`~/.local/share/lark/stt-models/`). Setup and bridge startup verify SHA-256
`6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396`.
The bridge does not download models or need an endpoint environment flag.
Install the model before upgrading the bridge binary.

Silero v5 runs on CPU with 512-sample windows, a 0.50 speech threshold,
120 ms minimum speech and a 320 ms silence tail. The separate release threshold
and tail keep brief dips from toggling speech state. The bridge subtracts the
tail to estimate acoustic end. Defaults in `endpoint.go` are measured from that
estimate, not added on top of the VAD tail:

| Threshold | Default | Rule |
| --- | ---: | --- |
| Decode a pause | 600 ms | One local Parakeet decode of the accumulated clip per pause |
| Strongly complete | 950 ms | Punctuated direct question (e.g. “why” + `?`) or a punctuated timer command with a duration |
| Other complete | 2,100 ms | Terminal `.`, `?` or `!`, enough content, no unfinished cue |
| Missing punctuation | 3,300 ms | Sufficient text but no final mark or unfinished cue |
| Patient / max silence after speech | 4,500 ms | Unfinished text, trailing filler, list/continuation cue, too few words, absent/failed probe, or story |

Three words count as enough content (leading question contractions like
“what’s” count as two words). Narrative starts, a span of at least 8 s from
first voice to estimated end, or at least 23 words select the patient tier.
Punctuation is only a hint: Parakeet can add it before a thought is finished,
or omit it on a finished turn. A complete-sounding prefix followed by a pause
longer than its tier can still be cut off.

The capture worker keeps reading PCM while decoding on the local GPU. New
speech cancels and invalidates the old guess, including during stop
acknowledgement, and PCM from both clauses remains in the same clip. A
nonempty successful decode is reused only when the same pause survives
through `rec_end`. Otherwise the worker decodes the final clip. No guess is
sent to OpenCode, and the mic indicator remains on while listening.

## Wire and follow-up behavior

- Firmware 0.9.5 and `/app` offer `caps.bridge_endpoint:true` in `hello`.
  Untargeted Talk, Say more, and enabled wake word use
  `{"op":"rec","action":"start","target":"","endpoint":"bridge"}` when
  negotiated. `/app` waits for `{"op":"capabilities","bridge_endpoint":true}`
  before asking the bridge to start. Targeted intercom and live calls
  retain their existing paths.
- Audio is `0x01` followed by 16 kHz mono s16le PCM between `rec_start`
  and `rec_end`. An automatic cut sends `rec stop`; extra PCM is accepted
  until `rec_end` or a 450 ms timeout. `cancelled:true` discards the capture.
  No speech discards at 7 s of audio; total capture is capped at 60 s.
  Clients also retain their ~8 s silence / 60 s watchdog and close the
  mic on Cancel, bridge stop, or disconnect. The mic is never silently
  reopened.
- Diagnostics `speech_end_to_commit_ms` measure estimated acoustic end to
  bridge `rec stop` (or `rec_end` for client Send/watchdog), excluding
  acknowledgement wait and final transcription. No end is recorded when
  speech is still heard at commit. `stt_ms` remains separate.
- **Say more** holds unpublished audio while the mic is open. It queues
  one normal follow-up behind the active reply, without interrupting or
  replaying the original prompt. Explicit **Stop** discards the queued
  follow-up. WebSocket tests cover the hold, prompt order, and Stop.

### Why steering stays off

Two disposable OpenCode 2.3.2 sessions used `opencode/claude-haiku-5-5`.
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
cd bridge
LARK_REPLAY_SILERO=/path/to/silero_vad_v5.onnx \
LARK_REPLAY_KOKORO=/path/to/kokoro-multi-lang-v1_0 \
LARK_BAKEOFF_MODEL="$stage/models/parakeet-unified-en-0.6b-Q8_0.gguf" \
CGO_CFLAGS="-I$stage/native/include" CGO_LDFLAGS="-L$stage/native/lib" \
  "$GO_BIN" test -tags parakeet -count=1 -v -run '^TestEndpointReplay$'
```

The replay saves no waveforms or transcripts. See the [earlier nine-clip
experiment](stt-speed.md#speculative-endpointing) for historical numbers.
