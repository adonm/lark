// Package proto is the generated v1 terminal wire protocol shared by the
// legacy bridge and the real-time edge. Go generate renders proto_gen.go from
// lark/protospec/spec.go; only the envelope helpers below are hand-written.
package proto

import (
	"encoding/binary"
	"errors"
	"io"
)

var (
	ErrMagic = errors.New("invalid protocol magic")
	ErrSize  = errors.New("protocol payload exceeds limit")
)

// Pack frames one message as the 8-byte little-endian envelope:
// magic, type, sequence, payload length.
func Pack(msgType uint8, seq uint16, payload []byte) []byte {
	pkt := make([]byte, ProtoHeaderSize+len(payload))
	pkt[0] = ProtoMagic
	pkt[1] = msgType
	binary.LittleEndian.PutUint16(pkt[2:4], seq)
	binary.LittleEndian.PutUint32(pkt[4:8], uint32(len(payload)))
	copy(pkt[ProtoHeaderSize:], payload)
	return pkt
}

// Unpack decodes one complete envelope from b.
func Unpack(b []byte) (msgType uint8, seq uint16, payload []byte, ok bool) {
	if len(b) < ProtoHeaderSize || b[0] != ProtoMagic {
		return 0, 0, nil, false
	}
	length := binary.LittleEndian.Uint32(b[4:8])
	if length > ProtoMaxPayload || len(b) < ProtoHeaderSize+int(length) {
		return 0, 0, nil, false
	}
	return b[1], binary.LittleEndian.Uint16(b[2:4]), b[ProtoHeaderSize : ProtoHeaderSize+int(length)], true
}

// ReadFrame reads one envelope from r. The payload buffer is freshly allocated
// by the caller's read path; callers that need zero allocations should use the
// datagram path instead.
func ReadFrame(r io.Reader) (msgType uint8, seq uint16, payload []byte, err error) {
	var hdr [ProtoHeaderSize]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	if hdr[0] != ProtoMagic {
		return 0, 0, nil, ErrMagic
	}
	msgType = hdr[1]
	seq = binary.LittleEndian.Uint16(hdr[2:4])
	length := binary.LittleEndian.Uint32(hdr[4:8])
	if length > ProtoMaxPayload {
		return 0, 0, nil, ErrSize
	}
	if length > 0 {
		payload = make([]byte, length)
		if _, err = io.ReadFull(r, payload); err != nil {
			return 0, 0, nil, err
		}
	}
	return msgType, seq, payload, nil
}
