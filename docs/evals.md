# Lark evals

Run from the repo root with Bash and Go 1.26:

```sh
source ./scripts/go-bin.sh
GO_BIN=$(resolve_go)
cd lark
"$GO_BIN" run . eval export
"$GO_BIN" run . eval score
"$GO_BIN" run . eval replay -fixture eval/testdata/synthetic.json -session Synthetic -turns 1
```

`export` uses only GET requests to the dedicated `127.0.0.1:49375` OpenCode,
with Basic auth from the private `lark.env`. It reads the message histories
of sessions titled for Cake, Mint, and greg, including older pages, without
opening the Lark database. It atomically replaces
`$XDG_DATA_HOME/lark/evals/sessions.json` (default `~/.local/share/lark/evals/`;
0700 directory, 0600 file). It does not download audio or attachments. The tool
refuses export destinations inside a Git worktree (including symlink targets).

The private file can contain children's prompts, responses, and tool content.
Never commit, upload, or paste it, or share derived per-turn fixtures. Only
synthetic examples belong in the repo. All modes put a concise table on stderr
and metrics-only JSON on stdout; use the session aggregates when sharing a
summary. `score` and `replay` accept `-fixture` for a saved snapshot or a
synthetic fixture.

- **Outcomes:** OpenCode's terminal idle outcome determines success, failure,
  or interruption; an explicit assistant abort is also counted separately.
  An interruption and abort can describe the same turn. Unsettled turns stay
  unfinished. The next-message gap runs from interruption/abort to the next
  user message (a negative number means the next prompt was already queued).
- **Timings / usage:** First token is the first reported model token (possibly
  reasoning) after user admission. Completion is admission to terminal idle,
  including failed and interrupted turns, not audio completion. Missing
  measurements are `null`. Token counts sum reported steps, with reasoning
  and cache counts separate; `token_steps` shows coverage. Scheduled
  compactions and their usage are excluded. Session latencies show
  nearest-rank p50/p95 and sample count in JSON.
- **Tools:** A round trip is one model step that calls tools, not one call;
  Code Mode can batch several calls per step. Reported tool failures and
  recognizable structured `error`/`isError` results count; prose or errors
  the agent swallowed are not inferred. Layout warnings count returned
  `layout_warnings` entries, if the agent returned them from Code Mode.

## Replay with a configured model

Offline `replay` uses an in-process stub with fixed logical timestamps, no
network and no quota. The offline suite checks the CLI, export, score and
session cleanup against synthetic fixtures and local stubs. Stub numbers
are not evidence of model quality or latency.

For background-build prompts, replay `eval/testdata/builds.json`
with `-session Synthetic -turns 1,2,3`. Replay scores the OpenCode turn;
it bypasses the Lark server, so it cannot measure the new local acknowledgement.
For the foreground comparison, run `"$GO_BIN" test -v -run '^TestBuildForegroundLatencyStub$' .`
from `lark`. It sends the same build request through the Lark server with and
without background routing, against a fake OpenCode with a fixed 250 ms model
delay. This measures server text reply latency, not time to audible speech
or real model speed.

For a real replay, prepare a disposable loopback OpenCode instance with its
own config/data/cache/state/workspace, the `lark` agent revision to test, and
**only** the `lark` MCP connected to a separate local Lark server with a temporary
database and no production devices. Use a mock companion/device if the
prompt needs on-screen state. Follow the [dedicated installation notes](../README.md#dedicated-opencode)
for config layout; make sure the MCP proxy inherits the test server's
`LARK_PORT` and `LARK_TOKEN`, not the installed ones.
Select the model in that OpenCode's `lark` agent configuration. The CLI uses
that selection as-is.

```sh
"$GO_BIN" run . eval replay \
  -fixture eval/testdata/synthetic.json -session Synthetic -turns 1 \
  -live http://127.0.0.1:49385 -workspace /path/to/disposable/workspace
```

If the disposable server requires auth, provide `LARK_EVAL_PASSWORD` in
the environment. Replaying a private export also requires
`LARK_EVAL_ALLOW_PRIVATE=1`: **the configured provider may receive and retain
the selected text**. Only opt in with permission. Replays send the stored
prompt as-is (including its original context header), in a fresh session for
each turn. They do not seed history or production device state, so compare the
same turns on two agent revisions rather than treating a contextual mismatch as
a quality regression. A run admits up to three prompts (4096 bytes each) and
limits each live attempt to 45 seconds by default (90 seconds max). Use the
test provider's own spend/output controls for a hard budget. On exit, replay
interrupts unfinished work and deletes only the sessions it created.

## Opt-in real-model release smoke

With a disposable Lark/OpenCode pair as above, point
`LARK_SMOKE_CONFIG_HOME` at its separate config root, containing
`lark/lark.env` with that pair's loopback ports, credentials and workspace.
From the repo root:

```sh
LARK_SMOKE_CONFIG_HOME=/path/to/disposable/config ./scripts/check.sh --real-model
```

This verifies Code Mode's four companion tools against a disposable virtual device
and cleans up the device and session. It uses the `lark` agent's configured
model and fails on real provider errors (including exhausted quota). This
smoke is not part of `--live` or CI.
