# Testing

`./scripts/check.sh` is the gate: shell checks, TypeScript typecheck and the
whole brain suite, Python setup tests, Go vet and the race suite, the tagged
Parakeet compile when a native bridge is available, host C tests under
ASan/UBSan, tracked-file hygiene, and `scripts/lint-shims.sh`. A final diagram
phase checks every `docs/diagrams/*.svg` for well-formed XML, `<title>` and
`<desc>`, and forbidden scripts, `foreignObject`, event handlers, or external
references. Run the gate before every push.

The brain suite includes `brain/src/e2e.test.ts`, which compiles the real Go
edge, starts it with deterministic fake speech models, starts the brain as a
separate process, and drives a virtual terminal over the real terminal protocol,
IPC, Delta Chat stub, HTTP API, and CLI. No GPU, model weights, relay, or
installed service is needed.

Firmware source is 0.24.0 on both boards. The edge and brain ship from the same
checkout through `lark deploy`.

## To change X, run Y

| Area | Smallest meaningful check |
| --- | --- |
| one brain module (`brain/src/*.ts`) | `cd brain && npm run typecheck && node --test src/*.test.ts` |
| one brain test | `cd brain && node --test --test-name-pattern 'name' src/module.test.ts` |
| one Go package | `cd lark && go test -race -tags sqlite_fts5 ./internal/edge/` |
| shell scripts | `bash -n scripts/*.sh scripts/lib/*.sh scripts/lark && shellcheck -x scripts/*.sh scripts/lib/*.sh scripts/lark` |
| setup Python (`scripts/setup/*.py`) | `python3 -m unittest discover -s scripts/tests` |
| protocol spec (`lark/protospec/`) | `cd lark && go generate ./protospec && go test ./protospec/` |
| firmware host logic (`firmware/main/*.c`) | `./scripts/check.sh --fast` (all host C tests, ~1 s) |
| firmware image | `./scripts/build.sh vocat` or `./scripts/build.sh box3` (incremental; `--force` rebuilds) |
| remote page (`web/remote/`) | `./scripts/check.sh --remote-ui` (whole gate, then the DOM suite) |
| diagrams (`docs/diagrams/`) | `./scripts/check.sh` (diagram hygiene) |
| deploy | `~/.local/share/lark/lark deploy` (builds and installs the edge, refreshes units, restarts, waits for readiness) |
| before push | `./scripts/check.sh` (full gate) |

Go invocations in this repo use `-tags sqlite_fts5` because the library indexer
needs the FTS5-enabled driver. The edge E2E builds without the tag and runs
with `-fake-models`, which replaces only the neural engines.

## `--fast`: the inner loop, not the gate

`./scripts/check.sh --fast [ref]` maps the diff against `ref` to whole areas and
runs only those:

- Base: explicit `ref`, else `LARK_FAST_BASE`, else the commit of the last
  green full gate (`.git/lark-check-green`, written by a bare
  `./scripts/check.sh`), else `origin/main`. The diff runs from the merge base.
- `lark/**` → Go build, vet, and the race suite.
- `brain/**` → brain typecheck and tests.
- `web/**` → remote page syntax.
- `firmware/**` → host C tests for both boards.
- `scripts/**` → shell checks and Python setup tests.
- No diff since the base means no work. CI never uses `--fast`.

`--fast` does not run `--remote-ui`, a firmware image build, diagram hygiene, or
the full shell and hygiene passes. Run the full gate before push.

## Flags and environment

| Flag | What it adds |
| --- | --- |
| `--remote-ui` | headless-Chromium DOM tests for the remote page (11 tests) |
| `--fast [ref]` | area-scoped inner loop against a base ref |

- `LARK_PLAYWRIGHT_MODULE` points `--remote-ui` at the Playwright module
  directory.
- `LARK_PARAKEET_NATIVE` reuses a verified native prefix for the Parakeet
  compile phase.
- `LARK_FAST_BASE` sets the default base for `--fast`.
- `TTS_MODEL_DIR` pointing at a missing pack makes Kokoro-dependent health
  reports unavailable (CI does this deliberately).

## CLI inspection

- `lark status` prints health, model states, the library plan, companions, and
  terminals.
- `lark doctor` checks the edge socket, the HTTP API, model states, the song
  recognizer, Delta Chat readiness, and orphaned accounts; it exits non-zero on
  failures.
- `lark companions [add <name> <form> | rm <id>]`, `lark terminals [attach |
  detach | rm]`, and `lark tunnels [stop <companion>]` drive the admin API.
- `lark capture <companion> [seconds]` records the voice-owner microphone
  through the listening path into a 16 kHz mono WAV under
  `~/.local/share/lark/debug/` (directory `0700`, file `0600`).
- `lark library --plan` prints the disk-aware plan as JSON;
  `lark library --download --build` runs bulk provisioning in `lark-library`.
- `lark ota <ip|host>` builds and pushes firmware from `lark-ops`; add
  `--no-build` to reuse the image.

## Measured timings

Local: 16 cores, Go 1.26.10, Node 26.5, warm build cache, Go test cache warm.

| Step | Warm |
| --- | ---: |
| shell | 0.6 s |
| brain (typecheck + 123 tests) | 19.4 s |
| python | 0.0 s |
| vet (+ `go mod verify`) | 0.5 s |
| go-race | 0.0 s |
| parakeet compile | 0.5 s |
| host C (ASan/UBSan, both boards) | 1.1 s |
| hygiene | 0.5 s |
| diagrams | 0.0 s |
| full gate | 23.2 s |

`--remote-ui` adds 3.1 s of DOM tests against Chromium in
`~/.cache/ms-playwright` (no browser download on the hot path); the measured
total was about 26 s.

The brain suite is most of the gate. The E2E test alone compiles the edge and
drives a full turn; the remaining tests cover the DB, router, Delta Chat,
stories, library, timers, portraits, OpenCode plugins, HTTP, and remote
controls.

## Current gaps

- STT accuracy and latency numbers come from synthetic Kokoro speech. Real
  child speech at room distance is still unmeasured; collect 2–3 minutes with
  `lark capture <companion> 60`, keep the WAVs local, and compare what was said
  with the transcript.
- Hardware QA, live Delta Chat, and tunnel checks need a board, a paired
  account, and `opentunnel`; they are not part of the offline gate.
- TTS/STT model readiness is reported as unavailable when `TTS_MODEL_DIR`
  points at a missing pack, as CI deliberately does.
