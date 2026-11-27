# Lark documentation

Start with the [README](../README.md) for setup and operator commands. Read
[ARCHITECTURE.md](../ARCHITECTURE.md) for the process map, voice pipeline, wire
protocol, data model, and invariants.

| Document | Contents |
| --- | --- |
| [protocol.md](protocol.md) | Generated terminal wire catalogue and constants. Change `lark/protospec/spec.go`, then run `go generate ./...`. |
| [endpointing.md](endpointing.md) | Edge endpoint policy, Smart Turn gating, listening mode, and synthetic replay results. |
| [stt-speed.md](stt-speed.md) | Local Parakeet measurements, artifact pins, and the native build. |
| [testing.md](testing.md) | Gate phases, `--fast`, per-area checks, and timings. |
| [diagrams/](diagrams/) | Hand-authored SVG diagrams used by the README and architecture. |
| [lark-upgrade changelog](../.opencode/skills/lark-upgrade/references/changelog.md) | Breaking changes and upgrade steps applied by the `lark-upgrade` skill. |

Keep a fact in one place and link to it. Diagrams use one palette and one font
stack, carry `<title>` and `<desc>`, and must pass the diagram hygiene phase in
`./scripts/check.sh`.
