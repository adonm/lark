# Testing

`./scripts/check.sh` is the gate: shell checks, Node tests, Go vet, the Go race
suite, the live-tag compile, host C tests under ASan/UBSan, and tracked-file
hygiene. Run it before every push. CI runs the same phases, with the race suite
split into three shards.

## To change X, run Y

| Area | Smallest meaningful check |
| --- | --- |
| one Go area (`lark/*.go`) | `./scripts/check.sh --fast` (~10 s) |
| one Go test | `cd lark && go test -race -tags sqlite_fts5 -run '^TestName$' .` |
| shell scripts | `bash -n scripts/*.sh && shellcheck -x scripts/*.sh` |
| OpenCode config (`config/opencode/`) | `node --test config/opencode/*.test.mjs` |
| firmware host logic (`firmware/main/*.c`) | `./scripts/check.sh --fast` (all host C tests, ~1 s) |
| firmware image | `./scripts/build.sh vocat` or `./scripts/build.sh box3` (incremental; `--force`, `--watch`) |
| remote page (`lark/web/remote/`) | `./scripts/check.sh --remote-ui` (DOM suite, ~3 s) |
| QA sim scenarios (`lark/qa/*.sim`) | `./scripts/check.sh --qa` (~12 s) |
| QA stub matrix (no models) | `./scripts/check.sh --e2e-stub` (~9 s) |
| board QA | `./scripts/check.sh --qa-hw` (~19 s) |
| whole product, virtual boards | `./scripts/check.sh --live` (≥2 min, real STT/TTS) |
| server deploy | `~/.local/share/lark/lark deploy` (builds, restarts, waits for models and terminals) |
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
  `firmware/` changed, shell checks only when `scripts/*.sh` changed.

No diff since the base means no build or tests. Typical single-file loop is
~10 s (28 tests for `endpoint.go`); a five-commit diff was ~10 s / 23 tests.
CI never uses `--fast`.

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
  `~/.local/share/lark/debug/` (directory `0700`, file `0600`, local only).

## Measured timings

Local: 16 cores, Go 1.26.10, warm build cache, Go test cache cleared before
each of three full runs (114.8 / 111.8 / 112.1 s).

| Step | Time |
| --- | --- |
| shell | 0.3 s |
| node | 0.1 s |
| vet (+ `go mod verify`) | 0.7 s |
| go-race | 109.3 s |
| live-tag compile | 0.5 s |
| host C | 1.0 s |
| hygiene | 0.0 s |
| full gate | ~112 s |

`--qa` adds 11.9 s (14.7 s total), `--qa-hw` adds 18.7 s (21.6 s total), and
`--remote-ui` adds 3.0 s. The race suite is ~97 % of the local gate.

CI keeps one offline job (187 s measured before this work). A three-way race
split was tried and rejected: its best case saved ~87 s (187 s → ~100 s),
under the 90 s bar, and every shard must reinstall ffmpeg for tests that
depend on it. That install took 45 s to over 5 minutes on the package mirror;
one shard ran 402 s, slower than the unsplit job. The firmware build dominates
the overall workflow anyway.

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
