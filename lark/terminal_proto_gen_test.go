package main

import (
	"bytes"
	"testing"

	"lark/protospec"
)

// Every fixture is produced by the generator from protospec samples. Decoding
// it with the generated Go codec and encoding it again must reproduce the
// exact bytes; firmware/tests/proto_test.c checks the same fixtures in C.
func TestGeneratedProtoFixtures(t *testing.T) {
	t.Parallel()
	if len(protospec.Fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range protospec.Fixtures {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			var got []byte
			switch f.TypeByte {
			case msgHello:
				m, ok := decodeTerminalHello(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgResume:
				m, ok := decodeTerminalResume(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgTelemetry:
				m, ok := decodeProtoTelemetry(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgPointer:
				m, ok := decodeProtoPointer(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgRecStart:
				m, ok := decodeProtoRecStart(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgRecEnd:
				m, ok := decodeProtoRecEnd(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgTTSAck:
				m, ok := decodeProtoTTSAck(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgPing:
				m, ok := decodeProtoPing(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgPong:
				m, ok := decodeProtoPong(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgWelcome:
				m, ok := decodeProtoWelcome(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgReject:
				m, ok := decodeProtoReject(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgRecCtrl:
				m, ok := decodeProtoRecCtrl(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgTTSCtrl:
				m, ok := decodeProtoTTSCtrl(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgChime:
				m, ok := decodeProtoChime(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgPower:
				m, ok := decodeProtoPower(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgVolume:
				m, ok := decodeProtoVolume(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgStatus:
				m, ok := decodeProtoStatus(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgVoiceOwner:
				m, ok := decodeProtoVoiceOwner(f.Bytes)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			default:
				t.Fatalf("no decoder for type 0x%02X", f.TypeByte)
			}
			if !bytes.Equal(got, f.Bytes) {
				t.Fatalf("re-encoded bytes differ\n got %x\nwant %x", got, f.Bytes)
			}
		})
	}
}

// Every decoder must reject a payload truncated at any prefix length, not just
// an empty hello. The fixture bytes are valid messages, so each strict prefix
// is a short payload.
func TestGeneratedProtoRejectsShortPayloads(t *testing.T) {
	t.Parallel()
	decode := func(typeByte uint8, b []byte) bool {
		switch typeByte {
		case msgHello:
			_, ok := decodeTerminalHello(b)
			return ok
		case msgResume:
			_, ok := decodeTerminalResume(b)
			return ok
		case msgTelemetry:
			_, ok := decodeProtoTelemetry(b)
			return ok
		case msgPointer:
			_, ok := decodeProtoPointer(b)
			return ok
		case msgRecStart:
			_, ok := decodeProtoRecStart(b)
			return ok
		case msgRecEnd:
			_, ok := decodeProtoRecEnd(b)
			return ok
		case msgTTSAck:
			_, ok := decodeProtoTTSAck(b)
			return ok
		case msgPing:
			_, ok := decodeProtoPing(b)
			return ok
		case msgPong:
			_, ok := decodeProtoPong(b)
			return ok
		case msgWelcome:
			_, ok := decodeProtoWelcome(b)
			return ok
		case msgReject:
			_, ok := decodeProtoReject(b)
			return ok
		case msgRecCtrl:
			_, ok := decodeProtoRecCtrl(b)
			return ok
		case msgTTSCtrl:
			_, ok := decodeProtoTTSCtrl(b)
			return ok
		case msgChime:
			_, ok := decodeProtoChime(b)
			return ok
		case msgPower:
			_, ok := decodeProtoPower(b)
			return ok
		case msgVolume:
			_, ok := decodeProtoVolume(b)
			return ok
		case msgStatus:
			_, ok := decodeProtoStatus(b)
			return ok
		case msgVoiceOwner:
			_, ok := decodeProtoVoiceOwner(b)
			return ok
		}
		return false
	}
	for _, f := range protospec.Fixtures {
		f := f
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			if !decode(f.TypeByte, f.Bytes) {
				t.Fatal("fixture does not decode")
			}
			for n := 0; n < len(f.Bytes); n++ {
				if decode(f.TypeByte, f.Bytes[:n]) {
					t.Fatalf("decoded a %d/%d byte prefix", n, len(f.Bytes))
				}
			}
		})
	}
}

// Retired fields stay in the layout so old firmware can parse the frame, but a
// decoded message re-encodes them as zero.
func TestGeneratedProtoZeroesRetiredFields(t *testing.T) {
	t.Parallel()
	zero := []struct {
		name    string
		typeByte uint8
		field   []int
	}{
		{"resume last_seq", msgResume, []int{17, 18, 19, 20}},
		{"welcome seq", msgWelcome, []int{16, 17, 18, 19}},
		{"volume auto", msgVolume, []int{2}},
		{"status seq/progress/kind", msgStatus, []int{0, 1, 2, 3, 5, 6}},
		{"voice owner seq", msgVoiceOwner, []int{0, 1, 2, 3}},
	}
	for _, tc := range zero {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fixture *protospec.Fixture
			for i := range protospec.Fixtures {
				if protospec.Fixtures[i].TypeByte == tc.typeByte {
					fixture = &protospec.Fixtures[i]
					break
				}
			}
			if fixture == nil {
				t.Fatal("no fixture")
			}
			raw := bytes.Clone(fixture.Bytes)
			for _, off := range tc.field {
				raw[off] = 0x5A
			}
			var got []byte
			switch tc.typeByte {
			case msgResume:
				m, ok := decodeTerminalResume(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgWelcome:
				m, ok := decodeProtoWelcome(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgVolume:
				m, ok := decodeProtoVolume(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgStatus:
				m, ok := decodeProtoStatus(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			case msgVoiceOwner:
				m, ok := decodeProtoVoiceOwner(raw)
				if !ok {
					t.Fatal("decode failed")
				}
				got = m.encode()
			}
			if !bytes.Equal(got, fixture.Bytes) {
				t.Fatalf("retired fields were not zeroed\n got %x\nwant %x", got, fixture.Bytes)
			}
		})
	}
}
