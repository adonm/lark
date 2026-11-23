# Testing

`./scripts/check.sh` is the gate: shell checks, Node tests, Python setup tests,
Go vet, the Go race suite, the live-tag compile, host C tests under ASan/UBSan,
tracked-file hygiene, and `scripts/lint-shims.sh`. Run it before every push. CI
runs the same offline phases in one job, plus a remote-page job, the stub
matrix, and firmware builds.

Live QA that targets the installed server requires
`LARK_QA_ALLOW_PRODUCTION=1`; `./scripts/check.sh --qa` and `--qa-hw` set it,
and `requireIsolatedQA` fails without it. Cleanup is id-scoped: tests delete
only rows they created, never by title or name.

## To change X, run Y

| Area | Smallest meaningful check |
| --- | --- |
| one Go area (`lark/*.go`) | `./scripts/check.sh --fast` (~10 s) |
| one Go test | `cd lark && go test -race -tags sqlite_fts5 -run '^TestName$' .` |
| shell scripts | `bash -n scripts/*.sh scripts/lib/*.sh && shellcheck -x scripts/*.sh scripts/lib/*.sh` |
| setup Python (`scripts/setup/*.py`) | `python3 -m unittest discover -s scripts/tests` |
| OpenCode config (`config/opencode/`) | `node --test config/opencode/*.test.mjs` |
| firmware host logic (`firmware/main/*.c`) | `./scripts/check.sh --fast` (all host C tests, ~1 s) |
| firmware image | `./scripts/build.sh vocat` or `./scripts/build.sh box3` (incremental; `--force` rebuilds) |
| remote page (`lark/web/remote/`) | `./scripts/check.sh --remote-ui` (whole gate, then a 3 s DOM suite) |
| QA sim scenarios (`lark/qa/*.sim`) | `./scripts/check.sh --qa` (~12 s) |
| QA stub matrix (no models) | `./scripts/check.sh --e2e-stub` (~9 s) |
| board QA | `./scripts/check.sh --qa-hw` (~19 s) |
| whole product, virtual boards | `./scripts/check.sh --live` (≥2 min, real STT/TTS) |
| server deploy | `~/.local/share/lark/lark deploy` (builds, installs, refreshes the OpenCode config, restarts, waits for models and terminals) |
| deployed service and OTA | `./scripts/check.sh --deployed` (read-only) |
| before push | `./scripts/check.sh` (full gate) |

Go invocations in this repo use `-tags sqlite_fts5`; live tests add
`-tags=live,sqlite_fts5`. `--live`, `--qa`, and `--deployed` build a tagged
`lark` binary and talk to a running server; `--qa` needs the installed binary
at `$XDG_DATA_HOME/lark/lark`.

## `--fast`: the inner loop, not the gate

`./scripts/check.sh --fast [ref]` builds, vets, and runs only the Go tests that
plausibly cover the diff against `ref`. It prints every test and the reason:

- Base: explicit `ref`, else `LARK_FAST_BASE`, else the commit of the last
  green full gate (`.git/lark-check-green`, written by a bare
  `./scripts/check.sh`), else `origin/main`. The diff runs from the merge base.
- A changed `_test.go` file runs the tests defined in it.
- A changed `lark/*.go` file runs tests whose names contain the file stem
  (`endpoint.go` → `Test*Endpoint*`).
- Otherwise, functions, types, vars, and consts declared in the diff run the
  tests whose bodies reference them. Symbols referenced by more than four tests
  are ignored as common words (`start`, `status`), and the number skipped is
  printed.
- Node tests run only when `config/opencode/` changed, host C tests only when
  `firmware/` changed, Python setup tests and shell checks only when
  `scripts/` changed. Build and vet always run.

No diff since the base means no build or tests. Typical single-file loop is
~10 s (28 tests for `endpoint.go`); a five-commit diff was ~10 s / 23 tests.
CI never uses `--fast`.

`--fast` limits:

- It does not run the `live` tag, `--e2e-stub`, `--qa`, `--qa-hw`,
  `--remote-ui`, `--deployed`, or a firmware image build.
- The file-stem rule runs only tests whose names contain the stem. The symbol
  rule sees declarations in the diff, not runtime references through
  interfaces, reflection, or string keys.
- A production Go change with no mapped test prints a warning and runs the
  whole package suite.
- A `go.mod`/`go.sum` or subpackage change runs every Go test.

## Flags and environment

| Flag | What it adds |
| --- | --- |
| `--qa` | `TestQASimSuite`: `lark/qa/*.sim` on virtual vocat and box3 boards |
| `--qa-hw` | the same plus `TestQAHardwareSuite`; hardware subtests skip when no board is attached |
| `--remote-ui` | headless-Chromium DOM tests for the remote page |
| `--e2e-stub` | the isolated stub bridge and all 11 non-hardware QA scenarios (~9 s) |
| `--live` | virtual companions and firmware clients, real Parakeet/Silero/Kokoro, stub model |
| `--e2e-real` | dedicated OpenCode with the configured model (spends quota) |
| `--deployed` | read-only installed-service and OTA probes |

- `LARK_QA_DC=1` enables the live Delta Chat QA tests. They are not part of
  `--qa`; run `cd lark && LARK_QA_DC=1 go test -tags=live,sqlite_fts5 -v
  -run '^TestQADeltaChat' .` with a paired account.
- `LARK_QA_LIBRARY=1` adds `TestQALocalLibraryBehaviour` after `--qa`
  (real model, slower).
- `LARK_QA_HW=1` is what `--qa-hw` sets.
- `LARK_FAST_BASE` sets the default base for `--fast`.
- `LARK_PARAKEET_NATIVE` reuses a verified native prefix for `--live`.
- `LARK_PLAYWRIGHT_MODULE` points `--remote-ui` at the Playwright module.
- `TTS_MODEL_DIR` pointing at a missing pack makes Kokoro tests skip (CI).

## CLI inspection

- `lark sim [--board vocat|box3] [--id sim-x] [--addr host:port] [--script f]`
  drives a virtual board against the running server; scripts are the ones in
  `lark/qa/`, and stdin works too.
- `lark device <terminal> status|tap X Y|tap-label NAME|mic file.wav|screen out.png`
  sends input to, or reads the screen of, a real board.
- `lark capture <companion> [seconds]` records the voice-owner microphone
  through the listening path into a 16 kHz mono WAV under
  `~/.local/share/lark/debug/` (directory `0700`, file `0600`). Captures are
  never uploaded to a third party; only the holder of a scoped link can play,
  transcribe, or delete them, and they are never used as fixtures.

## Measured timings

Local: 16 cores, Go 1.26.10, warm build cache, Go test cache cleared before
each full run. Before parallelising, three runs measured 141.9 / 141.7 /
141.8 s (`go-race` 139.0 / 139.0 / 139.1 s). After, three runs measured
29.2 / 27.6 / 27.1 s. The first run includes a 2 s live-tag compile after a
source change.

| Step | Before | After |
| --- | --- | --- |
| shell | 0.4 s | 0.4 s |
| node | 0.0 s | 0.0 s |
| vet (+ `go mod verify`) | 0.5 s | 0.5 s |
| go-race | 139.0 s | 24.5 s |
| live-tag compile | 0.4 s | 0.4 s |
| host C | 1.0 s | 1.0 s |
| hygiene | 0.0 s | 0.0 s |
| full gate | ~142 s | ~28 s |

The race suite is still most of the gate (~89 %). `--qa` adds 11.9 s,
`--qa-hw` adds 18.7 s, and `--remote-ui` adds 3.0 s of DOM tests that run
against the Chromium already in `~/.cache/ms-playwright` (no browser
download on the hot path; the module is resolved from `node_modules`). A
`--remote-ui` run therefore costs the full gate plus 3 s, which is what the
earlier 114.5 s observation measured: the gate, not the DOM suite.

### Parallel suite

`lark/parallel_test.go` documents the split: 259 top-level tests call
`t.Parallel`, and 19 stay in the sequential phase with `serial(t)`:

- 6 allocation tests: `testing.AllocsPerRun` panics in a parallel test.
- 5 environment tests (three capture, OTA, MCP stdio): `t.Setenv` forbids
  `t.Parallel`, and the environment is process state.
- 2 listen tests that shorten the package-level `listenWindow`.
- 5 measured tests whose wall-clock bounds (installed library latency,
  catalog-refresh pacing, playback backpressure) would be invalidated by
  parallel load.
- 1 restart test that closes a listener and rebinds the saved port.

The Go test runner completes every test without `t.Parallel` before it
starts the parallel batch, so the sequential phase is explicit, not luck.
Its long poles are `TestLibraryLatencyInstalled` (4.1 s of real SQLite
queries), the 2 s capture tests, and `TestKokoroReal`'s shared engine
(subtests stay serial because the sherpa engine is not safe to call
concurrently); `TestKokoroReal` is now the longest parallel test at ~9 s
under load.

Determinism checks after the change: `go test -race -tags sqlite_fts5
-count=3 -cpu 2,8 ./...` (six passes, 162 s) and `-count=5` at default
parallelism (116 s) both finished with 0 failures, as did one run
concurrent with a `--remote-ui` gate.

CI keeps one offline job and a separate remote-page job. Splitting the offline
job into race shards was tried and rejected; the measurements are in
[docs/qa-report.md](qa-report.md#ci-sharding-2026-11-23-historical).
Kokoro is skipped there when `TTS_MODEL_DIR` has no model pack.

## Fast stub QA loop and voice fixtures

`./scripts/check.sh --e2e-stub` builds the bridge with the `e2e` tag and runs
the whole non-hardware QA matrix against one isolated stub server. It prints
one row per scenario. No Parakeet, Silero, Kokoro, OpenCode, tunnelling or
Delta Chat installation is needed; a full run takes about 9 s.

The stub server (`lark/stub_e2e.go`) replaces only the model-backed pieces:
STT matches captured audio to a synthetic fixture by energy envelope and
returns that fixture's transcript, TTS returns a 900 ms tone, and Delta Chat
uses the in-memory RPC stub. Capture, endpoint policy, turns, routing,
rendering and storage stay on the production path.

Scenarios: voice turn, Stop, listening windows, menus on both boards, stories,
memories/voice/volume, timers, Delta Chat, DC intercom, two terminals, and
noise/`[silent]` robustness. Rule replies still execute real MCP tools through
the bridge endpoint, and assertions inspect state transitions, frames, model
prompts, timer rows and Delta Chat sends rather than answer quality.

Assertions that are weaker in stub mode:

- The RMS VAD stands in for Silero and there is no Smart Turn model, so
  complete turns commit on the 1.2 s cap instead of the 400 ms quiet path.
- The model is a substring rule table: tool execution is real, but choosing
  the right tool and wording is not tested.
- TTS is a tone and the firmware ack is paced, so audio quality and real
  playback timing are out of scope.
- Delta Chat never joins a relay: encryption, delivery and real pairing are
  out of scope.

`lark/qa/fixtures/voice/` holds ten synthetic Kokoro clips (short/long,
timer command, trailing "um", a 900 ms pause, comma list, function-word
ending, quiet speech over pink noise, noise only, 20 s monologue) generated by
`scripts/make-voice-fixtures.py`. `manifest.json` records origin, transcript,
probe progress and expected endpoint decision. Every committed clip is
synthetic; `TestVoiceFixtureManifestIsSynthetic` enforces that and the 5 MiB
budget. `TestVoiceFixturesEndpointPolicy` replays them through the production
endpoint policy (commit point, incomplete-word guard, pause window, noise
discard, monologue cap).

To test a real capture locally without publishing it: run
`lark capture <companion> <seconds>`, copy the WAV into the gitignored
`lark/qa/fixtures/local/`, add `local/manifest.json` with `origin: "local"`
and the same fields, then run
`cd lark && go test -tags sqlite_fts5 -run TestVoiceFixturesEndpointPolicy .`
