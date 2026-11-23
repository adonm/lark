// Package protospec is the single source of truth for the terminal wire
// protocol. The generator in this package renders firmware/main/proto.h, the
// Go codec in lark/terminal_proto_gen.go, the generated protocol catalogue in
// docs/protocol.md, and the cross-language fixtures.
//
// A message is a fixed prefix of scalar fields followed by length-prefixed
// string fields. That ordering is deliberate: it keeps the C structs packed
// with the same layout the firmware has always used.
package protospec

import "fmt"

// Kind is a wire field type.
type Kind int

const (
	U8 Kind = iota
	U16
	U32
	U64
	I16
	Bytes
	Str8
	Str16
)

func (k Kind) String() string {
	switch k {
	case U8:
		return "u8"
	case U16:
		return "u16"
	case U32:
		return "u32"
	case U64:
		return "u64"
	case I16:
		return "i16"
	case Bytes:
		return "bytes"
	case Str8:
		return "str8"
	case Str16:
		return "str16"
	}
	return "?"
}

func (k Kind) fixed() int {
	switch k {
	case U8:
		return 1
	case U16, I16:
		return 2
	case U32:
		return 4
	case U64:
		return 8
	case Str8:
		return 1
	case Str16:
		return 2
	}
	return 0
}

// Field is one field in a message.
type Field struct {
	Go      string // Go struct field, e.g. protoVersion
	C       string // C struct field, e.g. proto_version
	CLen    string // C length field for Str8/Str16
	Kind    Kind
	Count   int   // Bytes width
	Retired bool  // kept in the layout, always encoded as zero
	Sample  int64 // fixture value for numeric fields
	Str     string
	Bytes   []byte
	Doc     string
}

func (f Field) Size() int {
	if f.Kind == Bytes {
		return f.Count
	}
	return f.Kind.fixed()
}

func (f Field) Variable() bool { return f.Kind == Str8 || f.Kind == Str16 }

func (f Field) CType() string {
	switch f.Kind {
	case U8:
		return "uint8_t"
	case U16:
		return "uint16_t"
	case U32:
		return "uint32_t"
	case U64:
		return "uint64_t"
	case I16:
		return "int16_t"
	case Bytes:
		return fmt.Sprintf("uint8_t")
	case Str8:
		return "uint8_t"
	case Str16:
		return "uint16_t"
	}
	return "uint8_t"
}

// Message is one protocol message.
type Message struct {
	Name     string // identifier stem, e.g. RecStart
	GoName   string // Go type, e.g. protoRecStart
	CName    string // C struct, e.g. lark_rec_start_t
	MsgGo    string // Go type-byte constant, e.g. msgRecStart
	MsgC     string // C type-byte constant, e.g. LARK_MSG_REC_START
	TypeByte uint8
	ToDevice bool // Lark -> terminal
	Opaque   bool // variable payload with no fields (frame, TTS PCM)
	Doc      string
	Fields   []Field
}

func (m Message) FixedSize() int {
	n := 0
	for _, f := range m.Fields {
		n += f.Size()
	}
	return n
}

// Const is a protocol constant shared by both languages (or one, when Only is
// set).
type Const struct {
	Go    string // Go name; empty when Only == "c"
	C     string // C name; empty when Only == "go"
	Value uint64
	Flag  bool
	Bit   int // used when Flag is set
	Only  string
	Doc   string
}

func (c Const) HasGo() bool { return c.Only != "c" && c.Go != "" }
func (c Const) HasC() bool  { return c.Only != "go" && c.C != "" }

// Messages is ordered by type byte so the docs and generated maps stay stable.
var Messages = []Message{
	{
		Name: "Hello", GoName: "terminalHello", CName: "lark_hello_t",
		MsgGo: "msgHello", MsgC: "LARK_MSG_HELLO", TypeByte: 0x01,
		Doc: "Terminal identity, capabilities, panel geometry and string lengths.",
		Fields: []Field{
			{Go: "protoVersion", C: "proto_version", Kind: U8, Sample: 1},
			{Go: "kind", C: "kind", Kind: U8},
			{Go: "capsMask", C: "caps_mask", Kind: U32, Sample: 241},
			{Go: "displayFormats", C: "display_formats", Kind: U8, Sample: 1},
			{Go: "frameCodecs", C: "frame_codecs", Kind: U8, Sample: 15},
			{Go: "width", C: "width", Kind: U16, Sample: 360},
			{Go: "height", C: "height", Kind: U16, Sample: 360},
			{Go: "shape", C: "shape", Kind: U8},
			{Go: "flags", C: "flags", Kind: U8},
			{Go: "udpPort", C: "udp_port", Kind: U16, Sample: 40001},
			{Go: "terminalID", C: "id_len", CLen: "id_len", Kind: Str8, Str: "vocat-33d8"},
			{Go: "companionID", C: "comp_len", CLen: "comp_len", Kind: Str8},
			{Go: "board", C: "board_len", CLen: "board_len", Kind: Str8, Str: "esp-vocat"},
			{Go: "firmware", C: "fw_len", CLen: "fw_len", Kind: Str8, Str: "0.20.0"},
			{Go: "token", C: "tok_len", CLen: "tok_len", Kind: Str8, Str: "token"},
		},
	},
	{
		Name: "Resume", GoName: "terminalResume", CName: "lark_resume_t",
		MsgGo: "msgResume", MsgC: "LARK_MSG_RESUME", TypeByte: 0x02,
		Doc: "Resume token and terminal id. The old sequence field stays in the layout.",
		Fields: []Field{
			{Go: "protoVersion", C: "proto_version", Kind: U8, Sample: 1},
			{Go: "resumeToken", C: "resume_token", Kind: Bytes, Count: resumeTokenLen, Bytes: tokenSample()},
			{Go: "lastSeq", C: "last_seq", Kind: U32, Retired: true, Doc: "retired snapshot sequence"},
			{Go: "terminalID", C: "id_len", CLen: "id_len", Kind: Str8, Str: "vocat-33d8"},
		},
	},
	{
		Name: "Telemetry", GoName: "protoTelemetry", CName: "lark_telemetry_t",
		MsgGo: "msgTelemetry", MsgC: "LARK_MSG_TELEMETRY", TypeByte: 0x03,
		Doc: "Periodic device health snapshot.",
		Fields: []Field{
			{Go: "wifiRssi", C: "wifi_rssi", Kind: I16, Sample: -52},
			{Go: "batteryPct", C: "battery_pct", Kind: I16, Sample: 87},
			{Go: "charging", C: "charging", Kind: U8, Sample: 1},
			{Go: "cpuPct", C: "cpu_pct", Kind: U8, Sample: 42},
			{Go: "flags", C: "flags", Kind: U8, Sample: 0},
			{Go: "pad", C: "pad", Kind: U8, Sample: 0},
			{Go: "freeHeap", C: "free_heap", Kind: U32, Sample: 123456},
			{Go: "uptimeS", C: "uptime_s", Kind: U32, Sample: 789},
		},
	},
	{
		Name: "Pointer", GoName: "protoPointer", CName: "lark_pointer_t",
		MsgGo: "msgPointer", MsgC: "LARK_MSG_POINTER", TypeByte: 0x04,
		Doc: "Touch or gesture event with the shown frame revision.",
		Fields: []Field{
			{Go: "kind", C: "kind", Kind: U8, Sample: 1},
			{Go: "pad", C: "pad", Kind: U8},
			{Go: "x", C: "x", Kind: U16, Sample: 123},
			{Go: "y", C: "y", Kind: U16, Sample: 234},
			{Go: "revLo", C: "rev_lo", Kind: U16, Sample: 5678},
		},
	},
	{
		Name: "RecStart", GoName: "protoRecStart", CName: "lark_rec_start_t",
		MsgGo: "msgRecStart", MsgC: "LARK_MSG_REC_START", TypeByte: 0x06,
		Doc: "Terminal acknowledgement that recording started.",
		Fields: []Field{
			{Go: "session", C: "session", Kind: U16, Sample: 7},
			{Go: "target", C: "target_len", CLen: "target_len", Kind: Str8, Str: "bridge"},
		},
	},
	{
		Name: "RecEnd", GoName: "protoRecEnd", CName: "lark_rec_end_t",
		MsgGo: "msgRecEnd", MsgC: "LARK_MSG_REC_END", TypeByte: 0x07,
		Doc: "Terminal notification that recording ended.",
		Fields: []Field{
			{Go: "session", C: "session", Kind: U16, Sample: 7},
			{Go: "cancelled", C: "cancelled", Kind: U8, Sample: 1},
			{Go: "target", C: "target_len", CLen: "target_len", Kind: Str8, Str: "bridge"},
		},
	},
	{
		Name: "TTSAck", GoName: "protoTTSAck", CName: "lark_tts_ack_t",
		MsgGo: "msgTTSAck", MsgC: "LARK_MSG_TTS_ACK", TypeByte: 0x08,
		Doc: "Terminal acknowledgement that a TTS stream finished or aborted.",
		Fields: []Field{
			{Go: "aborted", C: "aborted", Kind: U8, Sample: 1},
			{Go: "audioID", C: "audio_id", Kind: U64, Sample: 0x0102030405060708},
		},
	},
	{
		Name: "Ping", GoName: "protoPing", CName: "lark_ping_t",
		MsgGo: "msgPing", MsgC: "LARK_MSG_PING", TypeByte: 0x0F,
		Doc: "TCP keepalive; the four payload bytes are echoed by PONG.",
		Fields: []Field{
			{Go: "payload", C: "payload", Kind: Bytes, Count: 4, Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
		},
	},
	{
		Name: "Pong", GoName: "protoPong", CName: "lark_pong_t",
		MsgGo: "msgPong", MsgC: "LARK_MSG_PONG", TypeByte: 0x10,
		Doc: "TCP keepalive reply.",
		Fields: []Field{
			{Go: "payload", C: "payload", Kind: Bytes, Count: 4, Bytes: []byte{0xAA, 0xBB, 0xCC, 0xDD}},
		},
	},
	{
		Name: "Welcome", GoName: "protoWelcome", CName: "lark_welcome_t",
		MsgGo: "msgWelcome", MsgC: "LARK_MSG_WELCOME", TypeByte: 0x81, ToDevice: true,
		Doc: "Session accepted: UDP endpoint, SSRC, limits, resume token and ids.",
		Fields: []Field{
			{Go: "protoVersion", C: "proto_version", Kind: U8, Sample: 1},
			{Go: "flags", C: "flags", Kind: U8, Sample: 1},
			{Go: "udpPort", C: "udp_port", Kind: U16, Sample: 40002},
			{Go: "ssrc", C: "ssrc", Kind: U32, Sample: 0x11223344},
			{Go: "maxMsgBytes", C: "max_msg_bytes", Kind: U32, Sample: 327680},
			{Go: "resumeWindowMS", C: "resume_window_ms", Kind: U32, Sample: 30000},
			{Go: "seq", C: "seq", Kind: U32, Retired: true, Doc: "retired snapshot sequence"},
			{Go: "resumeToken", C: "resume_token", Kind: Bytes, Count: resumeTokenLen, Bytes: tokenSample()},
			{Go: "terminalID", C: "id_len", CLen: "id_len", Kind: Str8, Str: "vocat-33d8"},
			{Go: "companionID", C: "comp_len", CLen: "comp_len", Kind: Str8, Str: "cake"},
		},
	},
	{
		Name: "Reject", GoName: "protoReject", CName: "lark_reject_t",
		MsgGo: "msgReject", MsgC: "LARK_MSG_REJECT", TypeByte: 0x82, ToDevice: true,
		Doc: "Session rejected with a reason and human-readable message.",
		Fields: []Field{
			{Go: "reason", C: "reason", Kind: U8, Sample: 6},
			{Go: "message", C: "msg_len", CLen: "msg_len", Kind: Str8, Str: "bad caps"},
		},
	},
	{
		Name: "Frame", GoName: "protoFrame", MsgGo: "msgFrame", MsgC: "LARK_MSG_FRAME",
		TypeByte: 0x83, ToDevice: true, Opaque: true,
		Doc: "Display frame; the payload is owned by the frame codec in frame.h.",
	},
	{
		Name: "RecCtrl", GoName: "protoRecCtrl", CName: "lark_rec_ctrl_t",
		MsgGo: "msgRecCtrl", MsgC: "LARK_MSG_REC_CTRL", TypeByte: 0x86, ToDevice: true,
		Doc: "Start, stop or cancel recording, with target flags and session.",
		Fields: []Field{
			{Go: "op", C: "op", Kind: U8, Sample: 1},
			{Go: "flags", C: "flags", Kind: U8, Sample: 65},
			{Go: "session", C: "session", Kind: U16, Sample: 7},
			{Go: "target", C: "target_len", CLen: "target_len", Kind: Str8, Str: "bridge"},
		},
	},
	{
		Name: "TTSCtrl", GoName: "protoTTSCtrl", CName: "lark_tts_ctrl_t",
		MsgGo: "msgTTSCtrl", MsgC: "LARK_MSG_TTS_CTRL", TypeByte: 0x87, ToDevice: true,
		Doc: "Start, finish or stop a TTS stream identified by audio_id.",
		Fields: []Field{
			{Go: "op", C: "op", Kind: U8, Sample: 1},
			{Go: "audioID", C: "audio_id", Kind: U64, Sample: 0x0102030405060708},
		},
	},
	{
		Name: "TTSPCM", GoName: "protoTTSPCM", MsgGo: "msgTTSPCM", MsgC: "LARK_MSG_TTS_PCM",
		TypeByte: 0x88, ToDevice: true, Opaque: true,
		Doc: "Raw 16 kHz mono little-endian PCM for the active TTS stream.",
	},
	{
		Name: "Chime", GoName: "protoChime", CName: "lark_chime_t",
		MsgGo: "msgChime", MsgC: "LARK_MSG_CHIME", TypeByte: 0x8A, ToDevice: true,
		Doc: "Play one named tone.",
		Fields: []Field{
			{Go: "kind", C: "kind", Kind: U8, Sample: 3},
		},
	},
	{
		Name: "Power", GoName: "protoPower", CName: "lark_power_t",
		MsgGo: "msgPower", MsgC: "LARK_MSG_POWER", TypeByte: 0x8B, ToDevice: true,
		Doc: "Set panel brightness, 0-100.",
		Fields: []Field{
			{Go: "level", C: "level", Kind: U8, Sample: 85},
		},
	},
	{
		Name: "Volume", GoName: "protoVolume", CName: "lark_volume_t",
		MsgGo: "msgVolume", MsgC: "LARK_MSG_VOLUME", TypeByte: 0x8C, ToDevice: true,
		Doc: "Set output volume, optionally persisting. The auto-mode byte is retired.",
		Fields: []Field{
			{Go: "level", C: "level", Kind: U8, Sample: 60},
			{Go: "persist", C: "persist", Kind: U8, Sample: 1},
			{Go: "auto", C: "auto_mode", Kind: U8, Retired: true, Doc: "retired auto-volume mode"},
			{Go: "terminalID", C: "id_len", CLen: "id_len", Kind: Str8, Str: "vocat-33d8"},
			{Go: "companionID", C: "comp_len", CLen: "comp_len", Kind: Str8, Str: "cake"},
		},
	},
	{
		Name: "Status", GoName: "protoStatus", CName: "lark_status_t",
		MsgGo: "msgStatus", MsgC: "LARK_MSG_STATUS", TypeByte: 0x8D, ToDevice: true,
		Doc: "Assistant state. Sequence, progress, kind and text stay in the layout.",
		Fields: []Field{
			{Go: "seq", C: "seq", Kind: U32, Retired: true, Doc: "retired state sequence"},
			{Go: "state", C: "state", Kind: U8, Sample: 3},
			{Go: "progress", C: "progress", Kind: U8, Retired: true, Doc: "retired progress percent"},
			{Go: "kind", C: "kind", Kind: U8, Retired: true, Doc: "retired prompt kind"},
			{Go: "text", C: "text_len", CLen: "text_len", Kind: Str16, Retired: true, Doc: "retired detail text"},
		},
	},
	{
		Name: "VoiceOwner", GoName: "protoVoiceOwner", CName: "lark_voice_owner_t",
		MsgGo: "msgVoiceOwner", MsgC: "LARK_MSG_VOICE_OWNER", TypeByte: 0x8E, ToDevice: true,
		Doc: "Companion voice ownership broadcast.",
		Fields: []Field{
			{Go: "seq", C: "seq", Kind: U32, Retired: true, Doc: "retired state sequence"},
			{Go: "companionID", C: "comp_len", CLen: "comp_len", Kind: Str8, Str: "cake"},
			{Go: "terminalID", C: "term_len", CLen: "term_len", Kind: Str8, Str: "vocat-33d8"},
		},
	},
}

// Constants are the protocol's scalar vocabulary. Values are shared; the Go
// and C identifiers differ and are kept explicit.
var Constants = []Const{
	{Go: "protoMagic", C: "LARK_PROTO_MAGIC", Value: 0x4C, Doc: "'L'"},
	{Go: "protoVersion1", C: "LARK_PROTO_VERSION", Value: 1},
	{Go: "protoHeaderSize", C: "LARK_PROTO_HDR_SIZE", Value: 8},
	{Go: "protoMaxPayload", C: "LARK_PROTO_MAX_PAYLOAD", Value: 320 * 1024},
	{Go: "resumeTokenLen", C: "LARK_RESUME_TOKEN_LEN", Value: resumeTokenLen},
	{Go: "resumeWindowMS", C: "LARK_RESUME_WINDOW_MS", Value: 30000},
	{C: "LARK_DEFAULT_DEVICE_PORT", Value: 8766, Only: "c"},
	{C: "LARK_DEFAULT_HTTP_PORT", Value: 8765, Only: "c"},

	{Go: "termKindHardware", C: "LARK_KIND_HARDWARE", Value: 0, Doc: "terminal kind"},
	{Go: "dispRGB565", C: "LARK_DISP_RGB565", Flag: true, Bit: 0, Doc: "display format"},
	{Go: "codecBitRaw", C: "LARK_CODEC_RAW", Flag: true, Bit: 0, Doc: "frame codec"},
	{Go: "codecBitRLE565", C: "LARK_CODEC_RLE565", Flag: true, Bit: 1},
	{Go: "codecBitLZ4", C: "LARK_CODEC_LZ4", Flag: true, Bit: 2},
	{Go: "codecBitJPEG", C: "LARK_CODEC_JPEG", Flag: true, Bit: 3},
	{Go: "codecBitAll", Value: 15, Only: "go"},
	{Go: "shapeRound", C: "LARK_SHAPE_ROUND", Value: 0, Doc: "screen shape"},
	{Go: "shapeRect", C: "LARK_SHAPE_RECT", Value: 1},
	{Go: "helloFlagIfFree", Value: 1, Only: "go", Doc: "hello flags"},

	{Go: "welcomeFlagBridgeEP", C: "LARK_WELCOME_FLAG_BRIDGE_EP", Flag: true, Bit: 0, Doc: "welcome flags"},

	{Go: "capBattery", C: "LARK_CAP_BATTERY", Flag: true, Bit: 0, Doc: "HELLO capability bits"},
	{Go: "capDrag", C: "LARK_CAP_DRAG", Flag: true, Bit: 4},
	{Go: "capPointerRev", C: "LARK_CAP_POINTER_REV", Flag: true, Bit: 5},
	{Go: "capBridgeEndpoint", C: "LARK_CAP_BRIDGE_ENDPOINT", Flag: true, Bit: 6},
	{Go: "capPanelSleep", C: "LARK_CAP_PANEL_SLEEP", Flag: true, Bit: 7},

	{Go: "rejectBadToken", C: "LARK_REJECT_BAD_TOKEN", Value: 1, Doc: "reject reasons"},
	{Go: "rejectBadVersion", C: "LARK_REJECT_BAD_VERSION", Value: 2},
	{Go: "rejectUnknownTerminal", C: "LARK_REJECT_UNKNOWN_TERMINAL", Value: 3},
	{Go: "rejectFull", C: "LARK_REJECT_FULL", Value: 4},
	{Go: "rejectOccupied", Value: 5, Only: "go"},
	{Go: "rejectBadCaps", C: "LARK_REJECT_BAD_CAPS", Value: 6},

	{Go: "statusIdle", C: "LARK_STATE_IDLE", Value: 0, Doc: "status states"},
	{Go: "statusListening", C: "LARK_STATE_LISTENING", Value: 1},
	{Go: "statusThinking", C: "LARK_STATE_THINKING", Value: 2},
	{Go: "statusSpeaking", C: "LARK_STATE_SPEAKING", Value: 3},
	{Go: "statusBlocked", Value: 4, Only: "go"},
	{Go: "statusDone", C: "LARK_STATE_DONE", Value: 5},
	{Go: "statusError", C: "LARK_STATE_ERROR", Value: 6},

	{Go: "ptrTap", C: "LARK_PTR_TAP", Value: 0, Doc: "pointer kinds"},
	{Go: "ptrDrag", C: "LARK_PTR_DRAG", Value: 1},
	{Go: "ptrUp", C: "LARK_PTR_UP", Value: 2},

	{Go: "recOpStop", C: "LARK_REC_STOP", Value: 0, Doc: "recording ops"},
	{Go: "recOpStart", C: "LARK_REC_START", Value: 1},
	{Go: "recOpCancel", C: "LARK_REC_CANCEL", Value: 2},
	{Go: "recTargetInter", C: "LARK_REC_TARGET_INTERCOM", Flag: true, Bit: 0, Doc: "recording target flags"},
	{Go: "recTargetListen", C: "LARK_REC_TARGET_LISTEN", Flag: true, Bit: 6},
	{Go: "recTargetBridgeE", C: "LARK_REC_TARGET_BRIDGE_EP", Flag: true, Bit: 7},

	{Go: "ttsOpStop", C: "LARK_TTS_STOP", Value: 0, Doc: "TTS ops"},
	{Go: "ttsOpStart", C: "LARK_TTS_START", Value: 1},
	{Go: "ttsOpEnd", C: "LARK_TTS_END", Value: 2},

	{Go: "chimeTap", C: "LARK_CHIME_TAP", Value: 0, Doc: "chime kinds"},
	{Go: "chimeThink", C: "LARK_CHIME_THINK", Value: 1},
	{Go: "chimeCancel", C: "LARK_CHIME_CANCEL", Value: 2},
	{Go: "chimeIntercom", C: "LARK_CHIME_INTERCOM", Value: 3},
	{Go: "chimeRing", C: "LARK_CHIME_RING", Value: 4},

	{Go: "rtpVersionByte", C: "LARK_RTP_VERSION_BYTE", Value: 0x80, Doc: "RTP (RFC 3550) constants"},
	{Go: "rtpHeaderSize", C: "LARK_RTP_HDR_SIZE", Value: 12},
	{Go: "rtpSamples", C: "LARK_RTP_SAMPLES", Value: 320},
	{Go: "rtpPCMBytes", C: "LARK_RTP_PCM_BYTES", Value: 640},
	{Go: "rtpPacketBytes", C: "LARK_RTP_PACKET_BYTES", Value: 652},
	{Go: "rtpPTRecPCM", C: "LARK_RTP_PT_REC_PCM", Value: 96},
}

const resumeTokenLen = 16

func tokenSample() []byte {
	b := make([]byte, resumeTokenLen)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}

// MessageForType returns the message with the given type byte.
func MessageForType(t uint8) (Message, bool) {
	for _, m := range Messages {
		if m.TypeByte == t {
			return m, true
		}
	}
	return Message{}, false
}

func CName(name string) string { return snakeUpper(name) }

func snakeUpper(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			prev := byte(0)
			if i > 0 {
				prev = s[i-1]
			}
			next := byte(0)
			if i+1 < len(s) {
				next = s[i+1]
			}
			if i > 0 && (prev < 'A' || prev > 'Z' || (next >= 'a' && next <= 'z')) {
				out = append(out, '_')
			}
			out = append(out, c)
		} else if c >= 'a' && c <= 'z' {
			out = append(out, c-('a'-'A'))
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
