# Local STT

Lark transcribes in-process Parakeet Vulkan for push-to-talk and whole-clip
captions. The edge runs one model and one session, serializes inference, and
prioritizes committed turns over speculative probes. Recognition is whole-clip,
not streaming.

| Property | Value |
| --- | --- |
| Weights | NVIDIA Unified EN 0.6B, ggml Q8_0 GGUF (225 Q8_0, 688 F32, 48 F16 tensors) |
| Engine | `transcribe.cpp` at `bcb639faa4c74a2300ba19e30360cb2355b1663a`, ggml Vulkan encoder, one CPU decoder thread |
| Head | Full attention for whole clips; greedy RNN-T, no beam or LM, up to 10 symbols per encoder frame |
| PCM | Mono 16 kHz, s16 divided by 32768; 150 ms of silence added at each end, then a 1.5 s minimum and 400 ms tail |
| Features | 128-bin Slaney log mel, 512 FFT, 25 ms Hann / 10 ms hop, 0.97 pre-emphasis, 0–8 kHz, per-feature normalization |
| Text | English-only, no language hint, no vocabulary biasing; model-native casing and punctuation |

## Speed on Ryzen 7 7700X / Radeon RX 6600

Six generated 16 kHz mono Kokoro clips (1.8–9.1 s), identical input for every
run. Nearest-rank percentiles; with six clips, p95 is the maximum, not a fleet
estimate. Whole-clip latency is the full call after one warm-up. CPU is process
time divided by wall time, with 100% representing one logical core. RSS is after
the run. The host also had other workloads; do not treat small differences as
significant.

| Run | CPU threads | Final p50 / p95 (ms) | CPU % | RSS (MiB) | Model init + first clip (ms) |
| --- | ---: | ---: | ---: | ---: | ---: |
| In-process Vulkan whole clip | 1 decoder | 96 / 133 | 62 | 195 | 388 + 239 |
| In-process Vulkan repeat | 1 decoder | 72 / 141 | 58 | 151 | 569 + 77 |

In-process Vulkan RSS high-water was 195 MiB. `amdgpu` reported 702 MiB of
allocated VRAM after the run: 697.4 MiB of weights and a 42.2 MiB encoder
scratch freed between calls. The bundled Sherpa/ORT runtime is still used for
Kokoro and Silero; its Parakeet CPU paths are not used.

## Build and runtime

The Go edge uses the published C API with one serialized model and session. The
static native build uses CMake/Ninja/C++17, `glslc`, Vulkan, and SPIR-V headers;
the reference build took about 38 s with four jobs. At runtime it needs the
Vulkan loader and a working driver, not X, Wayland, or an HTTP token service. A native driver crash takes down the edge, not only STT. Errors
do not silently move inference to CPU.

`scripts/setup-host.sh` downloads and verifies the source archive and GGUF by
SHA-256. The edge verifies the model again at startup. Deployed Sherpa/ORT
libraries come from the Go module verified against `go.sum` and carry a SHA-256
manifest in `$ORIGIN/lib`. The Vulkan loader and driver are system dependencies,
not bundled copies.

Sherpa 1.13.8 does not expose an AMD Linux GPU provider: MIGraphX would need a
custom ORT/Sherpa build and a supported AMD stack (RX 6600/gfx1032 is not in the
standard Radeon ROCm matrix).
ORT WebGPU/Dawn is not a stock Sherpa provider or a drop-in Vulkan EP.

For a scratch build, pass an existing build directory and a separate model
directory:

```sh
./scripts/build-parakeet.sh ~/.cache/lark/parakeet-native ~/.local/share/lark/stt-models
```

For headers outside the system prefix, set `CMAKE_PREFIX_PATH` to the SPIR-V
installation and add its include directory to `CXXFLAGS`.

## Artifact pins

Parakeet GGUF:
`handy-computer/parakeet-unified-en-0.6b-gguf` at
`7e948f21b7bdbac698d3318db9d350f1096f3b6c`,
`parakeet-unified-en-0.6b-Q8_0.gguf`,
`4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795`.

`transcribe.cpp` source archive:
`54fe2a5b2fb43662e6d5034585dd44012a67876ae382a49d124ab02d92162a88`.

Silero v5 and Smart Turn v3.2 pins are verified by `scripts/setup-host.sh`; the
endpointing policy that uses them is in [endpointing.md](endpointing.md).
