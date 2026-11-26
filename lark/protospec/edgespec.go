package protospec

import (
	"bytes"
	"fmt"
)

// The edge IPC and screen payloads are a second schema surface: the Go edge
// emits them and the TypeScript brain consumes them. Keep one declaration per
// interface here so the renderer and the generated TS cannot disagree.
//
// Screen field lists are checked against internal/edge/render.go by
// TestEdgeScreenMatchesSpec; that test fails when a Go struct field or JSON
// tag changes without updating this spec.

// EdgeField is one property of a generated TypeScript interface.
type EdgeField struct {
	Name     string
	Type     string
	Optional bool
	Doc      string
}

// EdgeType is one generated TypeScript interface.
type EdgeType struct {
	Name   string
	Doc    string
	Fields []EdgeField
}

var edgeTypes = []EdgeType{
	{
		Name: "EdgeModelState",
		Doc:  "One model's load state inside the ready snapshot.",
		Fields: []EdgeField{
			{Name: "state", Type: "string"},
			{Name: "error", Type: "string", Optional: true},
		},
	},
	{
		Name: "EdgeReady",
		Doc:  "Model load states, sent once per connection and on every change.",
		Fields: []EdgeField{
			{Name: "models", Type: "Record<string, EdgeModelState>"},
		},
	},
	{
		Name: "EdgeTerminal",
		Doc:  "A connected board snapshot.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "board", Type: "string"},
			{Name: "firmware", Type: "string"},
			{Name: "width", Type: "number"},
			{Name: "height", Type: "number"},
			{Name: "shape", Type: "string"},
			{Name: "ip", Type: "string"},
			{Name: "reconnects", Type: "number", Optional: true, Doc: "Reconnects since boot; older edges omit it."},
		},
	},
	{
		Name: "EdgeTelemetry",
		Doc:  "Periodic device health sample.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "wifi_rssi", Type: "number"},
			{Name: "battery_pct", Type: "number"},
			{Name: "charging", Type: "boolean"},
			{Name: "cpu_pct", Type: "number"},
			{Name: "free_heap", Type: "number"},
			{Name: "uptime_s", Type: "number"},
		},
	},
	{
		Name: "EdgeSpeculate",
		Doc:  "A provisional transcript that may still be replaced.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "seq", Type: "number"},
			{Name: "transcript", Type: "string"},
			{Name: "speech_at_ms", Type: "number"},
		},
	},
	{
		Name: "EdgeCommit",
		Doc:  "A finalized transcript.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "seq", Type: "number"},
			{Name: "transcript", Type: "string"},
			{Name: "speech_at_ms", Type: "number"},
			{Name: "listen_window", Type: "boolean"},
			{Name: "audio_ms", Type: "number"},
		},
	},
	{
		Name: "EdgeCancel",
		Doc:  "A turn abandoned before it committed.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "seq", Type: "number"},
			{Name: "reason", Type: "string"},
		},
	},
	{
		Name: "EdgePlayback",
		Doc:  "Playback finished, aborted, or underran.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "stream_id", Type: "number"},
			{Name: "aborted", Type: "boolean"},
			{Name: "underruns_delta", Type: "number"},
		},
	},
	{
		Name: "EdgePointer",
		Doc:  "A resolved touch action from a rendered screen.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "action", Type: "string"},
			{Name: "value", Type: "string"},
		},
	},
	{
		Name: "EdgeCaptureProgress",
		Doc:  "Seconds remaining in an active recording.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "capture_id", Type: "string"},
			{Name: "remaining_s", Type: "number"},
		},
	},
	{
		Name: "EdgeCaptureDone",
		Doc:  "A recording was written, or failed with an error.",
		Fields: []EdgeField{
			{Name: "terminal_id", Type: "string"},
			{Name: "capture_id", Type: "string"},
			{Name: "path", Type: "string"},
			{Name: "seconds", Type: "number"},
			{Name: "error", Type: "string", Optional: true},
		},
	},
	{
		Name: "EdgeHit",
		Doc:  "One touch target in the current frame.",
		Fields: []EdgeField{
			{Name: "ID", Type: "string"},
			{Name: "Label", Type: "string", Optional: true},
			{Name: "X1", Type: "number"},
			{Name: "Y1", Type: "number"},
			{Name: "X2", Type: "number"},
			{Name: "Y2", Type: "number"},
		},
	},
	{
		Name: "EdgeHitsReply",
		Doc:  "terminal.hits RPC reply.",
		Fields: []EdgeField{
			{Name: "rev", Type: "number"},
			{Name: "screen", Type: "string"},
			{Name: "mode", Type: "string"},
			{Name: "hits", Type: "EdgeHit[]"},
		},
	},
	{
		Name: "EdgeHealthReply",
		Doc:  "health RPC reply.",
		Fields: []EdgeField{
			{Name: "models", Type: "Record<string, EdgeModelState>"},
			{Name: "terminals", Type: "number"},
		},
	},
	{
		Name: "RecStartOptions",
		Doc:  "Optional terminal.rec_start fields.",
		Fields: []EdgeField{
			{Name: "listenWindow", Type: "boolean", Optional: true},
			{Name: "windowS", Type: "number", Optional: true},
			{Name: "followUpGraceMs", Type: "number", Optional: true},
			{Name: "companionName", Type: "string", Optional: true},
		},
	},
	{
		Name: "TuneNote",
		Doc:  "One note of a terminal.play_tune melody.",
		Fields: []EdgeField{
			{Name: "note", Type: "string"},
			{Name: "beats", Type: "number"},
		},
	},
	{
		Name: "ScreenItem",
		Fields: []EdgeField{
			{Name: "id", Type: "string"},
			{Name: "label", Type: "string"},
			{Name: "detail", Type: "string", Optional: true},
			{Name: "icon", Type: "string", Optional: true},
			{Name: "action", Type: "string", Optional: true},
			{Name: "value", Type: "string", Optional: true},
		},
	},
	{
		Name: "ScreenChoice",
		Fields: []EdgeField{
			{Name: "id", Type: "string"},
			{Name: "label", Type: "string"},
			{Name: "value", Type: "string"},
		},
	},
	{
		Name: "ScreenVoice",
		Fields: []EdgeField{
			{Name: "id", Type: "string"},
			{Name: "label", Type: "string"},
		},
	},
	{
		Name: "ScreenRow",
		Fields: []EdgeField{
			{Name: "title", Type: "string"},
			{Name: "text", Type: "string"},
		},
	},
	{
		Name: "Screen",
		Doc:  "Declarative terminal.render payload. Unknown fields are ignored; an empty kind renders nothing.",
		Fields: []EdgeField{
			{Name: "kind", Type: "string"},
			{Name: "portrait_dir", Type: "string", Optional: true},
			{Name: "companion_name", Type: "string", Optional: true},
			{Name: "companion_form", Type: "string", Optional: true},
			{Name: "status", Type: "string", Optional: true},
			{Name: "caption", Type: "string", Optional: true},
			{Name: "listen_window", Type: "boolean", Optional: true},
			{Name: "page", Type: "number", Optional: true},
			{Name: "title", Type: "string", Optional: true},
			{Name: "heading", Type: "string", Optional: true},
			{Name: "body", Type: "string", Optional: true},
			{Name: "body_scale", Type: "number", Optional: true},
			{Name: "invite", Type: "string", Optional: true},
			{Name: "level", Type: "number", Optional: true},
			{Name: "current", Type: "string", Optional: true},
			{Name: "items", Type: "ScreenItem[]", Optional: true},
			{Name: "choices", Type: "ScreenChoice[]", Optional: true},
			{Name: "voices", Type: "ScreenVoice[]", Optional: true},
			{Name: "rows", Type: "ScreenRow[]", Optional: true},
		},
	},
}

// edgeEvents lists every edge -> brain event in wire order.
var edgeEvents = []EdgeField{
	{Name: "ready", Type: "EdgeReady"},
	{Name: "terminal.connected", Type: "EdgeTerminal"},
	{Name: "terminal.disconnected", Type: "{ terminal_id: string }"},
	{Name: "terminal.telemetry", Type: "EdgeTelemetry"},
	{Name: "turn.speculate", Type: "EdgeSpeculate"},
	{Name: "turn.commit", Type: "EdgeCommit"},
	{Name: "turn.cancel", Type: "EdgeCancel"},
	{Name: "turn.not_caught", Type: "{ terminal_id: string }"},
	{Name: "pointer.action", Type: "EdgePointer"},
	{Name: "playback.done", Type: "EdgePlayback"},
	{Name: "capture.progress", Type: "EdgeCaptureProgress"},
	{Name: "capture.done", Type: "EdgeCaptureDone"},
	{Name: "disconnected", Type: "{ reason: string }", Doc: "The local socket closed; the client reconnects on its own."},
}

const edgeHeader = "// Code generated by lark/protospec/cmd/protogen from edgespec.go; DO NOT EDIT.\n"

func renderEdgeTS() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(edgeHeader)
	for _, t := range edgeTypes {
		b.WriteString("\n")
		if t.Doc != "" {
			fmt.Fprintf(&b, "/** %s */\n", t.Doc)
		}
		fmt.Fprintf(&b, "export interface %s {\n", t.Name)
		for _, f := range t.Fields {
			if f.Doc != "" {
				fmt.Fprintf(&b, "  /** %s */\n", f.Doc)
			}
			name := f.Name
			if !isTSIdentifier(name) {
				name = fmt.Sprintf("%q", name)
			}
			optional := ""
			if f.Optional {
				optional = "?"
			}
			fmt.Fprintf(&b, "  %s%s: %s;\n", name, optional, f.Type)
		}
		b.WriteString("}\n")
	}
	b.WriteString("\nexport interface EdgeEventMap {\n")
	for _, ev := range edgeEvents {
		if ev.Doc != "" {
			fmt.Fprintf(&b, "  /** %s */\n", ev.Doc)
		}
		name := ev.Name
		if !isTSIdentifier(name) {
			name = fmt.Sprintf("%q", name)
		}
		fmt.Fprintf(&b, "  %s: %s;\n", name, ev.Type)
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

func isTSIdentifier(s string) bool {
	for i, r := range s {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return s != ""
}
