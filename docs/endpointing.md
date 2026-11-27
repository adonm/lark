# Endpointing

Lark keeps the mic open through thinking pauses and uses local transcription to
decide when to send. **Send** ends recording now; **Cancel** discards it.
Firmware advertises `caps.edge_endpoint` when audio is ready, so the edge owns
endpointing rather than the board.

## Policy

`scripts/setup-host.sh` installs `silero_vad_v5.onnx` from the Sherpa ONNX
`asr-models` release at `$XDG_DATA_HOME/lark/stt-models/` (default
`~/.local/share/lark/stt-models/`). Setup and server startup verify SHA-256
`6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396`.
The server does not download models at runtime.

Silero v5 runs on CPU with 512-sample windows, a 0.50 speech threshold, 120 ms
minimum speech, and a 320 ms silence tail. A frame only counts as speech when
VAD is positive **and** its RMS clears an adaptive room floor. The floor is the
10th percentile of the first second of non-speech frames, then tracks upward
slowly and downward instantly during non-speech. VAD owns the speech decision;
the floor only vetoes noise, including VAD false positives and its silence tail.

| Rule | Default | Behaviour |
| --- | ---: | --- |
| Probe pause | 250 ms | Decode one growing clip, serialized with committed decodes |
| Speculation | 180 ms | Start a probe when Smart Turn ≥ 0.40; cancel on new voiced audio or changed text |
| Quiet commit | 400 ms | Gated silence; Smart Turn ≥ 0.90 and no incomplete trailing word |
| Cap | 1,200 ms | Gated silence; commits regardless of transcript completeness |
| Hesitation cap | 2,200 ms | Short or incomplete text with Smart Turn below 0.25 waits this long |
| Monologue | 20 s | Continuous gated speech commits even without a pause |
| No-speech discard | 6 s | A tap window with no gated speech closes without a turn |
| Listen window | 30 s | Re-arms after each reply; tap or this cap closes it |
| Capture cap | 60 s | Any capture ends at 60 s |

Silence is measured from the last frame that was both VAD-positive and above
the floor. Transcript growth cannot extend it. Parakeet probes can hallucinate
words on room noise, so probe text only advances when the decoded audio
contained gated speech; a first nonempty decode counts, and later probes must
not reduce the word count.

The quiet-commit path refuses to cut a turn that ends in a conjunction, article,
possessive, preposition, auxiliary, copula, filler, or trailing comma:
`his, her, the, a, an, and, but, or, so, because, if, to, of, in, on, at, is,
was, are, um, uh…` all wait for the cap or the next word. A segment with at least
one second of gated speech but no usable transcript speaks one short cached line
("I didn't catch that.", "Sorry, what was that?", "Say that again?"), re-arms
the window, and never creates an agent turn. A cough or a door does not trigger
it.

## Listening mode

Listening is a single boolean per companion (`patch.companion.listening`).
After a reply the voice-owner terminal opens a 30 s window. For 10 s after the
reply finishes, follow-ups need no name. After that a new turn must contain the
companion's name in the first five words; a normalized edit distance up to 2
tolerates STT near misses ("Kate" for Cake). Nameless speech is dropped silently
as background chatter. When the name is present but the rest is empty or filler,
the companion says "didn't catch that" and re-arms. `[silent]` replies and
ignored speech both re-arm through normal playback completion.

`GET /api/diagnostics` reports each terminal's `listening`, `busy`, `speaking`,
and current drawer state.

## Wire behavior

- Firmware that advertises `caps.edge_endpoint: true` in `hello` uses the
  edge-owned endpoint path for tap-to-talk.
- Microphone audio is RTP over UDP: payload type 96, 320 samples of 16 kHz mono
  s16le per packet, 20 ms apart. The edge consumes it between `rec_start` and
  `rec_end`.
- An automatic cut sends `rec stop`. A terminal Send or its watchdog also ends
  the capture; `cancelled:true` discards it. The server discards a capture with
  no gated speech after 6 s of audio and caps any capture at 60 s. The firmware
  watchdog sends at 8 s of silence or 60 s and closes the mic on Cancel, server
  stop, or disconnect. The mic is never silently reopened.
- **Say more** holds unpublished audio while the mic is open. It queues one
  normal follow-up behind the active reply, without interrupting or replaying
  the original prompt. Explicit **Stop** discards the queued follow-up. Lark does
  not use provider-side steering or auto-replay prompts after audio or tools;
  the queued follow-up is the only path.

## Measured policy

Forty generated Kokoro clips (Bella, Heart, Puck), with no recordings of
children or other real speakers: 17 uninterrupted short requests and questions,
15 paused turns (0.7–4 s gaps), three trailing-filler turns, and five
multi-clause stories. The last audible 10 ms block is the estimated true end.
Percentiles are nearest-rank and include observed Vulkan decode time before a
decision, not network or device acknowledgement.

| Replay path | Premature commits | End → commit p50 / p95 | Short turns p50 / p95 |
| --- | ---: | ---: | ---: |
| Silero + tuned local decode | 0/40 | 3,360 / 4,560 ms | 2,130 / 3,380 ms |

No test turn was missed. The policy keeps the fastest tier narrow and spends
2.1 s on other apparently complete text. The run used 72 pause decodes across
the 40 fixtures (one per detected pause ≥ 600 ms). Results are synthetic, not a
field benchmark.
