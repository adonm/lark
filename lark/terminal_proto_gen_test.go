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

func TestGeneratedProtoRejectsShortPayloads(t *testing.T) {
	t.Parallel()
	for _, f := range protospec.Fixtures {
		f := f
		if len(f.Bytes) == 0 {
			continue
		}
		if _, ok := decodeTerminalHello(f.Bytes[:0]); ok && f.TypeByte == msgHello {
			t.Fatal("hello decoded from an empty payload")
		}
	}
}
