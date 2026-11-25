# Local STT bake-off

**Lark runs in-process Parakeet Vulkan for push-to-talk and whole-clip captions.**
It uses the same engine and weights as Hex 3.4.0, ran headless on the RX 6600,
and was within 1.5× of Hex's fast whole-clip median. Lark no longer
uses the Hex service, HTTP API, streaming, or settings. The measurements below
retain Hex as a historical baseline. Capable clients now use
[Silero endpointing with speculative local decoding](endpointing.md).

> **Historical.** These measurements came from the v1 Go server's bake-off
> harness. The engines moved unchanged to `lark-edge`; the harness itself was
> removed with the monolith, so commands below are not runnable.

## Equivalence

| | Hex 3.4.0 (historical) | In-process Parakeet | Sherpa ONNX Runtime CPU (bench only) |
|---|---|---|---|
| Weights | NVIDIA Unified EN 0.6B (`d4ac992`), exact handy-computer GGUF below | Same SHA-256 and revision | Same named NVIDIA checkpoint and RNN-T head; separate FP32 and ONNX int8 conversions, not the Hex GGUF. NVIDIA source revision not confirmed. |
| Precision and engine | `transcribe.cpp` 0.4.1 fork at `bcb639faa4c74a2300ba19e30360cb2355b1663a`, ggml/Vulkan encoder + one CPU decoder thread; GGUF Q8_0 (225 Q8_0, 688 F32, 48 F16 tensors) | Same source revision, compiler options for the backend/shader subset and GGUF; one decoder thread | FP32 or ONNX dynamic int8, which is not ggml's block quantization; CPU only |
| Head and search | Full attention for whole clips; greedy RNN-T (no beam or LM), up to 10 symbols/encoder frame; not TDT/CTC | Same | Full attention offline and greedy RNN-T, likewise up to 10 symbols/frame; different runtime, numerics and frontend |
| Streaming | Buffered `(left, chunk, right) = (5.60, 1.04, 1.04)` s; ≥2.08 s initial window, carries RNN-T state; native finalize adds 1.04 s of right context, and Hex feeds 0.4 s model padding | No live stream; full attention on the available clip | Buffered presets `240/560/1120 ms` specify **chunk + right**, not chunk: `(5.60,.08,.16)`, `(5.60,.16,.40)`, `(5.60,.56,.56)` s. Not Hex's largest preset. |
| PCM and VAD | Mono 16 kHz input, s16 `/32768`; no resample for device PCM. Whole-clip: Lark adds 150 ms at each end of voiced PCM; Hex pads to ≥1.5 s and appends another 400 ms. No VAD or segmentation before decoding the measured whole clips; keyboard dictation and meetings are separate paths. | Same PCM scaling and padding; no added VAD in STT | Same source PCM, without Hex clip padding in the CPU runs. No programmatic VAD used. |
| Features | 128-bin Slaney log mel, 512 FFT, 25 ms Hann / 10 ms hop, pre-emphasis 0.97, 0–8 kHz, constant STFT pad, per-feature normalization. GGUF metadata stores `dither=1e-5`, but the runtime does not pass it to `MelConfig` (no added noise). | Same code and metadata | NeMo metadata selects 128 bins and per-feature normalization (over each window in streaming); 25/10 ms, 0.97 pre-emphasis, no dither. Kaldi-native/librosa feature implementation is not identical. |
| Text | English-only model; no runtime language hint; model outputs casing/punctuation. Raw API, no LLM formatting. Hex trims whitespace, keeps the repetition guard's partial result, and strips known trailing subtitle credits. Host vocabulary biasing is off. | Same tested raw output path and filter, no Hex settings import | Model-native English casing/punctuation, no Hex filters or vocabulary rescoring. Accuracy equivalence not established. |

Sources: [Hex v3.4.0](https://github.com/anomalyco/hex/tree/v3.4.0)
(`src/transcription/{artifacts,gguf_session,local_transcriber,service}.rs`,
`src/transcription.rs`, `src/local_api/{streams,transcriptions}.rs`);
[transcribe.cpp model notes](https://github.com/anomalyco/transcribe.cpp/blob/bcb639faa4c74a2300ba19e30360cb2355b1663a/docs/models/parakeet-unified-en-0.6b.md);
[Sherpa 1.13.8 source](https://github.com/k2-fsa/sherpa-onnx/tree/v1.13.8/scripts/nemo/parakeet-unified-en-0.6b).
Host selects `parakeet_unified_en`, `en`, GPU device 0; local API processing,
timings and vocabulary biasing were off. Measured stream timing matches the
default window.

## Speed on Ryzen 7 7700X / Radeon RX 6600

Six generated 16 kHz mono Kokoro clips (1.8–9.1 s), identical input files
for every engine. Nearest-rank percentiles; with six clips, p95 is the
maximum, not a fleet estimate. Whole-clip latency measures the full call
after one warm-up on the first clip. Stream latency is from start of clip
to first nonempty **pre-finish** text, and from the scheduled last sample
to final (includes any decoder backlog). Hex posts live in 160 ms blocks;
Sherpa feeds live in 20 ms blocks, then appends 1.4 s of silence without
waiting on the audio clock. CPU is process time / wall time, with 100%
representing one logical core. RSS is after the run. `Init + first` uses
a fresh model process and the host's existing caches, not a machine reboot.
The host also had other workloads; do not treat small timing differences
as significant.

| Engine | CPU threads | Final p50 / p95 (ms) | CPU % | RSS (MiB) | Model init + first clip (ms) |
|---|---:|---:|---:|---:|---:|
| Hex Vulkan whole clip | 1 decoder | 67 / 605 | 29 | 196 | not restarted; first call 65 |
| Hex repeat | 1 decoder | 607 / 615 | 18 | 201 | not restarted; first call 611 |
| In-process Vulkan whole clip | 1 decoder | 96 / 133 | 62 | 195 | 388 + 239 |
| In-process Vulkan repeat | 1 decoder | 72 / 141 | 58 | 151 | 569 + 77 |
| Sherpa int8 offline | 1 | 379 / 835 | 100 | 1,598 | 2,657 + 255 |
| Sherpa int8 offline | 4 | 142 / 300 | 506 | 1,594 | 2,742 + 106 |
| Sherpa int8 offline | 8 | 155 / 331 | 958 | 1,580 | 2,707 + 102 |
| Sherpa int8 offline | 16 | 353 / 656 | 1,276 | 1,574 | 2,683 + 213 |
| Sherpa FP32 offline | 1 | 919 / 1,849 | 84 | 3,025 | 7,594 + 544 |
| Sherpa FP32 offline | 4 | 799 / 1,862 | 244 | 3,031 | 7,784 + 672 |
| Sherpa FP32 offline | 8 | 1,392 / 1,961 | 392 | 3,027 | 11,382 + 1,266 |
| Sherpa FP32 offline | 16 | 1,922 / 3,479 | 543 | 3,028 | 9,035 + 1,647 |

Hex runs showed a fast ~65 ms mode and slower ~600 ms calls; the probe
cannot attribute the latter to queueing, residency or other host activity.
Use the fast mode as the replacement bar. The tagged server reached
readiness in 876 ms with an isolated DB and prewarmed weights; no X/Wayland
display was set. Its six whole-clip transcripts matched Hex byte-for-byte
on a separate pass; not an accuracy study. In-process Vulkan RSS
high-water was 195 MiB, and amdgpu
reported 702 MiB of allocated VRAM after the run (697.4 MiB of weights;
largest logged encoder scratch 42.2 MiB, freed between calls). Hex's
per-process GPU counters were not readable; its identical GPU weights are
~697 MiB before scratch. Sherpa allocated no GPU memory; int8 offline peak
RSS was ~1.6 GiB, FP32 peak ~4.2 GiB.

| Stream | Threads | First partial p50 / p95 (ms) | Audio end → final p50 / p95 (ms) | Flush p50 / p95 (ms) | CPU % | RSS (MiB) | Init (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|
| Hex Vulkan | 1 decoder | 2,181 / 2,182 (5/6) | 65 / 97 | 64 / 93 | 5 | 199 | already loaded |
| Sherpa int8 240 ms | 4 | 1,358 / 1,713 | 8,414 / 15,043 | 3,651 / 3,775 | 463 | 988 | 1,351 |
| Sherpa int8 240 ms | 8 | 1,193 / 1,342 | 6,146 / 8,995 | 2,698 / 3,515 | 971 | 1,010 | 1,161 |
| Sherpa int8 240 ms | 16 | 1,581 / 2,581 | 10,331 / 73,846 | 4,652 / 25,814 | 1,221 | 1,002 | 1,195 |
| Sherpa int8 560 ms | 4 | 1,483 / 4,366 | 4,673 / 10,386 | 3,002 / 3,932 | 406 | 1,021 | 3,727 |
| Sherpa int8 560 ms | 8 | 1,763 / 6,716 | 6,180 / 17,139 | 3,934 / 11,079 | 631 | 986 | 1,402 |
| Sherpa int8 560 ms | 16 | 1,470 / 2,428 | 5,870 / 9,975 | 3,262 / 4,355 | 1,381 | 1,021 | 1,446 |
| Sherpa int8 1120 ms | 4 | 1,361 / 1,924 | 949 / 1,583 | 835 / 1,336 | 220 | 1,001 | 1,224 |
| Sherpa int8 1120 ms | 8 | 1,297 / 1,853 | 692 / 774 | 620 / 773 | 352 | 1,005 | 1,173 |
| Sherpa int8 1120 ms | 16 | 1,314 / 1,876 | 989 / 1,295 | 873 / 1,276 | 714 | 1,034 | 1,209 |

The 1.8 s clip finished before Hex could emit a partial. The tight
Sherpa presets start emitting earlier but re-encode much more audio,
which can leave seconds of queued compute after capture. No service
restart, exclusive run, or GPU/system package installation was done.

## Speculative endpointing

The initial nine-fixture experiment let Silero
trigger one whole-clip decode per pause at 500 or 700 ms of estimated
silence. Decode availability is delayed on the audio clock by the measured
call time. Speech resumption invalidates the guess; the existing timer
fast-path and 4.5 s fallback still control whether a turn ends. This replay
does not include device stop acknowledgement or prove safety on real users.

| Path | Premature | End of speech → final p50 / p95 (ms) |
|---|---:|---:|
| Board RMS + in-process whole clip | 6/9 | 1,568 / 1,620 (3 correct stops) |
| Silero + in-process whole clip, no hints | 0/9 | 4,674 / 4,701 |
| Silero + speculative in-process at 500 ms | 0/9 | 4,540 / 4,570 |
| Silero + speculative in-process at 700 ms | 0/9 | 4,540 / 4,570 |

500 and 700 ms both hid the final decode, but made no material difference
to median turn latency versus the earlier ~4.61 s Silero + Hex-stream
replay. Both timer requests did finish in 920–970 ms, down from ~4.6 s
with no hints. Switching engines does not solve general turn-taking.
The prior wide-prefix shortcut cut 3/9 fixtures early. The current policy and
expanded synthetic replay are in [endpointing.md](endpointing.md).

## Build and reproduction

The Go backend uses the published C API with one serialized model and one
fresh session per inference, keeping weights resident but not GPU scratch.
The static native build took about 38 s with four jobs in scratch, using
CMake/Ninja/C++17, `glslc`, Vulkan and SPIR-V headers. The stripped binary
grew from 39.6 to 49.8 MiB (+10.2 MiB) and now links `libvulkan.so.1`.
At runtime it needs the Vulkan loader and working driver, not Hex, Xvfb, a stream lease,
an HTTP token or any other service. A native driver crash now takes down
the server, not only STT. Errors do not silently move inference to CPU.
The production setup verifies the source archive and GGUF by SHA-256.
The server verifies the model again at startup. The build records the
static library digests; deployed Sherpa/ORT libraries come from the Go module
verified against `go.sum` and have a SHA-256 manifest in `$ORIGIN/lib`.
The Vulkan loader and driver are system dependencies, not bundled copies.
The bundled Sherpa/ORT is still used for Kokoro and Silero. Sherpa 1.13.8
does not expose an AMD Linux GPU provider: MIGraphX would need a custom
ORT/Sherpa build and a supported AMD stack (RX 6600/gfx1032 is not in the
standard Radeon ROCm matrix; legacy ROCm EP was removed in ORT 1.23).
ORT WebGPU/Dawn is not a stock Sherpa provider or a drop-in Vulkan EP.

For a scratch build, pass an existing scratch directory and a separate
model dir to `scripts/build-parakeet.sh`, then use its `native/` prefix.
For headers outside the system prefix, set `CMAKE_PREFIX_PATH` to the
SPIR-V installation and add its include directory to `CXXFLAGS`.

```sh
cd lark
CGO_CFLAGS="-I$stage/native/include" CGO_LDFLAGS="-L$stage/native/lib" \
  go test -tags parakeet -c -o "$stage/bakeoff"
mkdir -p "$stage/audio"
LARK_BAKEOFF=fixtures LARK_BAKEOFF_AUDIO="$stage/audio" \
  LARK_REPLAY_KOKORO="$kokoro" "$stage/bakeoff" -test.v -test.run '^TestSTTBakeoff$'
env -u DISPLAY -u WAYLAND_DISPLAY GGML_VK_VISIBLE_DEVICES=0 \
  LARK_BAKEOFF=parakeet LARK_BAKEOFF_AUDIO="$stage/audio" \
  LARK_BAKEOFF_MODEL="$model" "$stage/bakeoff" -test.v -test.run '^TestSTTBakeoff$'
```

Use `LARK_BAKEOFF=sherpa|sherpa-stream` for the other
paths, `LARK_BAKEOFF_THREADS=1|4|8|16` for Sherpa, and
`LARK_BAKEOFF_ONNX_EXT=.onnx` for FP32.
The v1 endpoint replay harness was removed with the Go monolith; the policy now
runs in `lark/internal/edge/`.

## Artifact pins

Hex GGUF: `handy-computer/parakeet-unified-en-0.6b-gguf` at
`7e948f21b7bdbac698d3318db9d350f1096f3b6c`,
`parakeet-unified-en-0.6b-Q8_0.gguf`,
`4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795`.

Official k2-fsa [`asr-models` release](https://github.com/k2-fsa/sherpa-onnx/releases/tag/asr-models)
archive SHA-256 values verified against downloaded bytes. Each filename
starts `sherpa-onnx-nemo-parakeet-unified-en-0.6b-`:

| Filename suffix | SHA-256 |
|---|---|
| `int8-non-streaming.tar.bz2` | `99f63605b3a85a54c250c0869670a687b7d6598a47bf2421515e1f839a76e150` |
| `int8-streaming-240ms.tar.bz2` | `dead05a9149f6f02d373f3eb4553c74af4f189ff74889b5e210b35ca102655da` |
| `int8-streaming-560ms.tar.bz2` | `dd2c2698f102eafbf0ee54bdfd7cd842ec00fa6cf2475cbbb048887f794ff52e` |
| `int8-streaming-1120ms.tar.bz2` | `4788229a6dd03be33f8243ccee48e33a8d15df7b448cb99150b0ccddd1b02d74` |

FP32 isn't a GitHub release archive. The Sherpa maintainer's
[`sherpa-onnx-nemo-parakeet-unified-en-0.6b-non-streaming`](https://huggingface.co/csukuangfj2/sherpa-onnx-nemo-parakeet-unified-en-0.6b-non-streaming/tree/e69c9fb5322532e9995b116b6f1e4b34ab0c4b13)
at `e69c9fb5322532e9995b116b6f1e4b34ab0c4b13` supplies these files:

| File | Verified SHA-256 |
|---|---|
| `encoder.onnx` | `b96ad329f7190b672655c7c5020e191098705d4a736508012b89ad8ef820315a` |
| `encoder.weights` | `c054b2932ee13aa39ed84982a399822f3193311fb6fd8e1e204566722614bd87` |
| `decoder.onnx` | `41136a36f1673de81e1b5182f2aad09791815ec6cc9861ebaf2c56b1a374474f` |
| `joiner.onnx` | `680ddf13ab1f8a75b8d9f831980124b826dbbf4640f92cdfd30b72d4d2e811a4` |
| `tokens.txt` | `dc0b4584ab2e4ddbf888425c076c61b736e7356a015250db7d307e6f1a8188ff` |
