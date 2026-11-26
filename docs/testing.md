# Testing

`./scripts/check.sh` is the gate: shell checks, TypeScript typecheck and the
whole brain suite, Python setup tests, Go vet and the race suite, host C tests
under ASan/UBSan, tracked-file hygiene, and `scripts/lint-shims.sh`. Run it
before every push. CI runs the same offline phases in one job, a remote-page
job, and firmware builds.

The brain suite includes `brain/src/e2e.test.ts`, which compiles the real Go
edge, starts it with deterministic fake speech models, starts the brain as a
separate process, and drives a virtual terminal over the real terminal protocol,
IPC, Delta Chat stub, HTTP API, and CLI. No GPU, model weights, relay, or
installed service is needed.

## To change X, run Y

| Area | Smallest meaningful check |
| --- | --- |
| one brain module (`brain/src/*.ts`) | `cd brain && npm run typecheck && node --test src/*.test.ts` (~18 s) |
| one brain test | `cd brain && node --test --test-name-pattern 'name' src/module.test.ts` |
| one Go package | `cd lark && go test -race -tags sqlite_fts5 ./internal/edge/` |
| shell scripts | `bash -n scripts/*.sh scripts/lib/*.sh scripts/lark && shellcheck -x scripts/*.sh scripts/lib/*.sh scripts/lark` |
| setup Python (`scripts/setup/*.py`) | `python3 -m unittest discover -s scripts/tests` |
| protocol spec (`lark/protospec/`) | `cd lark && go generate ./protospec && go test ./protospec/` |
| firmware host logic (`firmware/main/*.c`) | `./scripts/check.sh --fast` (all host C tests, ~1 s) |
| firmware image | `./scripts/build.sh vocat` or `./scripts/build.sh box3` (incremental; `--force` rebuilds) |
| remote page (`web/remote/`) | `./scripts/check.sh --remote-ui` (whole gate, then a 3 s DOM suite) |
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

`--fast` does not run `--remote-ui`, a firmware image build, or the full shell
and hygiene passes. Run the full gate before push.

## Flags and environment

| Flag | What it adds |
| --- | --- |
| `--remote-ui` | headless-Chromium DOM tests for the remote page (11 tests) |
| `--fast [ref]` | area-scoped inner loop against a base ref |

- `LARK_PLAYWRIGHT_MODULE` points `--remote-ui` at the Playwright module
  directory.
- `LARK_PARAKEET_NATIVE` reuses a verified native prefix for the parakeet
  compile phase.
- `LARK_FAST_BASE` sets the default base for `--fast`.
- `TTS_MODEL_DIR` pointing at a missing pack makes Kokoro-dependent health
  reports unavailable (CI does this deliberately).

## CLI inspection

- `lark status` prints health, model states, the library plan, companions, and
  terminals.
- `lark doctor` checks the edge socket, the HTTP API, model states, Delta Chat
  readiness, and orphaned accounts; it exits non-zero on failures.
- `lark companions [add <name> <form> | rm <id>]`, `lark terminals [attach |
  detach | rm]`, and `lark tunnels [stop <companion>]` drive the admin API.
- `lark capture <companion> [seconds]` records the voice-owner microphone
  through the listening path into a 16 kHz mono WAV under
  `~/.local/share/lark/debug/` (directory `0700`, file `0600`). Captures are
  never uploaded to a third party; only the holder of a scoped link can play,
  transcribe, or delete them, and they are never used as fixtures.
- `lark library --plan` prints the disk-aware plan as JSON;
  `lark library --download --build` runs bulk provisioning in `lark-edge`.
- `lark ota <ip|host>` builds and pushes firmware from `lark-edge`; add
  `--no-build` to reuse the image.

## Measured timings

Local: 16 cores, Go 1.26.10, Node 26.5, warm build cache, Go test cache cleared
before the full run.

| Step | Warm | Cold |
| --- | --- | --- |
| shell | 0.5 s | 0.5 s |
| brain (typecheck + 83 tests) | 19.3 s | 19.6 s |
| python | 0.0 s | 0.0 s |
| vet (+ `go mod verify`) | 0.5 s | 0.6 s |
| go-race | 0.0 s | 6.1 s |
| parakeet compile | 0.8 s | 0.8 s |
| host C (ASan/UBSan, both boards) | 1.1 s | 1.1 s |
| hygiene | 0.4 s | 0.4 s |
| full gate | 23.1 s | 29.5 s |

`--remote-ui` adds 3.1 s of DOM tests against the Chromium already in
`~/.cache/ms-playwright` (no browser download on the hot path). A
`--remote-ui` run therefore costs the full gate plus 3 s; the measured total
was 26.0 s warm.

The brain suite is most of the gate. The E2E test alone compiles the edge and
drives a full turn; the remaining tests are unit tests over the DB, router,
Delta Chat, stories, library, timers, portraits, OpenCode plugins, HTTP, and
remote controls.

CI keeps one offline job, a separate remote-page job, and the firmware build.
Kokoro-dependent health checks in CI run with `TTS_MODEL_DIR` pointing at a
missing pack, so model readiness is reported unavailable rather than loaded.
