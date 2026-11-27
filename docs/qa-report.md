# Lark QA Report

## Current state

As of 2026-11-27 on `main`:

- `./scripts/check.sh` is the gate. It passes shell syntax and ShellCheck,
  TypeScript typecheck and the whole brain suite (including the real
  Go-edge ↔ TS-brain cross-process E2E), Python setup tests, Go vet and the
  race suite, the tagged Parakeet compile when a native bridge is available,
  strict GCC/ASan/UBSan host tests for both board layouts, `scripts/lint-shims.sh`,
  and tracked-file hygiene.
- `./scripts/check.sh --fast [ref]` runs only the areas the diff touches. It is
  the inner loop, never the gate, and does not replace a full run before push.
- `./scripts/check.sh --remote-ui` runs the full gate and then the 11
  headless-Chromium DOM tests for the remote page in about 3 s.
- CI runs the offline gate and the remote-page job on every push and pull
  request. A third job builds both ESP-IDF images on pushes to `main`, on
  `workflow_dispatch`, and nightly, so toolchain drift surfaces before release.
- Firmware source is 0.23.0 on vocat and box3. The edge and brain ship from the
  same checkout through `lark deploy`; no model weights, GPU, relay, or
  installed service are needed for the offline checks.

Open gaps:

- STT accuracy and latency numbers come from synthetic Kokoro speech. Real
  child speech at room distance is still unmeasured; collect 2–3 minutes with
  `lark capture <companion> 60`, keep the WAVs local, and compare what was said
  with the transcript.
- Hardware QA, live Delta Chat, and tunnel checks need a board, a paired
  account, and `opentunnel`; they are not part of the offline gate.
- TTS/STT model readiness is reported as unavailable when `TTS_MODEL_DIR`
  points at a missing pack, as CI deliberately does.
