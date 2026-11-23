package main

//go:generate go run ./protospec/cmd/protogen

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const (
	defaultDevPort      = "8766"
	frameTag       byte = 3
)

// Message types, sizes, field offsets and codecs live in
// terminal_proto_gen.go, generated from protospec/spec.go. This file only
// holds the envelope helpers and the typed wrappers used by the rest of lark.

var (
	errProtoMagic = errors.New("invalid protocol magic")
	errProtoSize  = errors.New("protocol payload exceeds limit")
	errProtoShort = errors.New("short protocol payload")
)

func packProtoPacket(msgType uint8, seq uint16, payload []byte) []byte {
	pkt := make([]byte, protoHeaderSize+len(payload))
	pkt[0] = protoMagic
	pkt[1] = msgType
	binary.LittleEndian.PutUint16(pkt[2:4], seq)
	binary.LittleEndian.PutUint32(pkt[4:8], uint32(len(payload)))
	copy(pkt[protoHeaderSize:], payload)
	return pkt
}

func unpackProtoPacket(b []byte) (msgType uint8, seq uint16, payload []byte, ok bool) {
	if len(b) < protoHeaderSize || b[0] != protoMagic {
		return 0, 0, nil, false
	}
	length := binary.LittleEndian.Uint32(b[4:8])
	if length > protoMaxPayload || len(b) < protoHeaderSize+int(length) {
		return 0, 0, nil, false
	}
	return b[1], binary.LittleEndian.Uint16(b[2:4]), b[protoHeaderSize : protoHeaderSize+int(length)], true
}

func readProtoFrame(r io.Reader) (msgType uint8, seq uint16, payload []byte, err error) {
	var hdr [protoHeaderSize]byte
	if _, err = io.ReadFull(r, hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	if hdr[0] != protoMagic {
		return 0, 0, nil, errProtoMagic
	}
	msgType = hdr[1]
	seq = binary.LittleEndian.Uint16(hdr[2:4])
	length := binary.LittleEndian.Uint32(hdr[4:8])
	if length > protoMaxPayload {
		return 0, 0, nil, errProtoSize
	}
	if length > 0 {
		payload = make([]byte, length)
		if _, err = io.ReadFull(r, payload); err != nil {
			return 0, 0, nil, err
		}
	}
	return msgType, seq, payload, nil
}

func appendStr8(dst []byte, s string) []byte {
	n := min(len(s), 255)
	dst = append(dst, byte(n))
	return append(dst, s[:n]...)
}

func appendStr16(dst []byte, s string) []byte {
	n := min(len(s), 65535)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(n))
	return append(dst, s[:n]...)
}

func readStr8(b []byte, off *int) (string, bool) {
	if *off >= len(b) {
		return "", false
	}
	n := int(b[*off])
	*off++
	if *off+n > len(b) {
		return "", false
	}
	s := string(b[*off : *off+n])
	*off += n
	return s, true
}

func readStr16(b []byte, off *int) (string, bool) {
	if *off+2 > len(b) {
		return "", false
	}
	n := int(binary.LittleEndian.Uint16(b[*off : *off+2]))
	*off += 2
	if *off+n > len(b) {
		return "", false
	}
	s := string(b[*off : *off+n])
	*off += n
	return s, true
}

func parseTerminalHello(b []byte) (terminalHello, error) {
	h, ok := decodeTerminalHello(b)
	if !ok {
		return terminalHello{}, errProtoShort
	}
	return h, nil
}

func encodeTerminalHello(h terminalHello) []byte { return h.encode() }

func parseTerminalResume(b []byte) (terminalResume, error) {
	r, ok := decodeTerminalResume(b)
	if !ok {
		return terminalResume{}, errProtoShort
	}
	return r, nil
}

// The resume frame still carries the retired last-state sequence as zero so
// the 0.20.0 firmware's fixed layout keeps decoding.
func encodeTerminalResume(protoVer uint8, token [resumeTokenLen]byte, terminalID string) []byte {
	return terminalResume{protoVersion: protoVer, resumeToken: token, terminalID: terminalID}.encode()
}

func helloCapsFromMask(mask uint32) terminalCaps {
	return terminalCaps{
		Battery:        mask&capBattery != 0,
		Drag:           mask&capDrag != 0,
		PointerRev:     mask&capPointerRev != 0,
		BridgeEndpoint: mask&capBridgeEndpoint != 0,
		PanelSleep:     mask&capPanelSleep != 0,
	}
}

func capsMaskFromCaps(c terminalCaps) uint32 {
	var m uint32
	if c.Battery {
		m |= capBattery
	}
	if c.Drag {
		m |= capDrag
	}
	if c.PointerRev {
		m |= capPointerRev
	}
	if c.BridgeEndpoint {
		m |= capBridgeEndpoint
	}
	if c.PanelSleep {
		m |= capPanelSleep
	}
	return m
}

func encodeWelcome(flags uint8, udpPort uint16, ssrc, maxMsgBytes, resumeWinMS uint32, token [resumeTokenLen]byte, terminalID, companionID string) []byte {
	return protoWelcome{
		protoVersion:   protoVersion1,
		flags:          flags,
		udpPort:        udpPort,
		ssrc:           ssrc,
		maxMsgBytes:    maxMsgBytes,
		resumeWindowMS: resumeWinMS,
		resumeToken:    token,
		terminalID:     terminalID,
		companionID:    companionID,
	}.encode()
}

func encodeReject(reason uint8, message string) []byte {
	return protoReject{reason: reason, message: message}.encode()
}

func parseTelemetry(b []byte) (terminalTelemetry, bool) {
	m, ok := decodeProtoTelemetry(b)
	if !ok {
		return terminalTelemetry{}, false
	}
	rssi := int(m.wifiRssi)
	batt := int(m.batteryPct)
	charging := m.charging != 0
	cpu := float64(m.cpuPct)
	freeHeap := uint64(m.freeHeap)
	uptime := uint64(m.uptimeS)
	var battPtr *int
	if batt >= 0 {
		battPtr = new(batt)
	}
	return terminalTelemetry{
		WiFi:     new(rssi),
		Battery:  battPtr,
		Charge:   new(charging),
		CPU:      new(math.Min(100, cpu)),
		FreeHeap: new(freeHeap),
		Uptime:   new(uptime),
	}, true
}

func parsePointer(b []byte) (evName string, x, y int, rev uint16, ok bool) {
	m, ok := decodeProtoPointer(b)
	if !ok {
		return "", 0, 0, 0, false
	}
	switch m.kind {
	case ptrTap:
		evName = "tap"
	case ptrDrag:
		evName = "drag"
	case ptrUp:
		evName = "up"
	default:
		return "", 0, 0, 0, false
	}
	return evName, int(m.x), int(m.y), m.revLo, true
}

func encodePointer(kind uint8, x, y, revLo uint16) []byte {
	return protoPointer{kind: kind, x: x, y: y, revLo: revLo}.encode()
}

func parseRecStart(b []byte) (session uint16, target string, ok bool) {
	m, ok := decodeProtoRecStart(b)
	return m.session, m.target, ok
}

func parseRecEnd(b []byte) (session uint16, cancelled bool, target string, ok bool) {
	m, ok := decodeProtoRecEnd(b)
	return m.session, m.cancelled != 0, m.target, ok
}

func parseTTSAck(b []byte) (aborted bool, audioID uint64, ok bool) {
	m, ok := decodeProtoTTSAck(b)
	return m.aborted != 0, m.audioID, ok
}

func encodeTTSAck(aborted bool, audioID uint64) []byte {
	var a uint8
	if aborted {
		a = 1
	}
	return protoTTSAck{aborted: a, audioID: audioID}.encode()
}

func encodeRecCtrl(op, flags uint8, session uint16, target string) []byte {
	return protoRecCtrl{op: op, flags: flags, session: session, target: target}.encode()
}

func encodeTTSCtrl(op uint8, audioID uint64) []byte {
	return protoTTSCtrl{op: op, audioID: audioID}.encode()
}

func encodeChimePayload(kind string) (byte, bool) {
	switch kind {
	case "tap":
		return chimeTap, true
	case "think":
		return chimeThink, true
	case "cancel":
		return chimeCancel, true
	case "intercom", "celebrate":
		return chimeIntercom, true
	case "ring":
		return chimeRing, true
	default:
		return 0, false
	}
}

func encodeVolumeMsg(level int, persist, auto bool, terminalID, companionID string) []byte {
	var p, a uint8
	if persist {
		p = 1
	}
	if auto {
		a = 1
	}
	return protoVolume{
		level:       uint8(min(100, max(0, level))),
		persist:     p,
		auto:        a,
		terminalID:  terminalID,
		companionID: companionID,
	}.encode()
}

// The payload keeps its 9-byte layout for 0.20.0 firmware; sequence, progress,
// kind and text are retired, so only the state byte carries information.
func encodeStatus(state uint8) []byte { return protoStatus{state: state}.encode() }

func encodeVoiceOwner(companionID, terminalID string) []byte {
	return protoVoiceOwner{companionID: companionID, terminalID: terminalID}.encode()
}

func encodeRTPPacket(dst []byte, pt uint8, seq uint16, ts, ssrc uint32, pcm []byte) int {
	dst[0] = rtpVersionByte
	dst[1] = pt & 0x7F
	binary.BigEndian.PutUint16(dst[2:4], seq)
	binary.BigEndian.PutUint32(dst[4:8], ts)
	binary.BigEndian.PutUint32(dst[8:12], ssrc)
	copy(dst[rtpHeaderSize:], pcm)
	return rtpHeaderSize + len(pcm)
}

func decodeRTPHeader(pkt []byte) (pt uint8, seq uint16, ts, ssrc uint32, ok bool) {
	if len(pkt) < rtpHeaderSize || pkt[0] != rtpVersionByte {
		return 0, 0, 0, 0, false
	}
	return pkt[1] & 0x7F,
		binary.BigEndian.Uint16(pkt[2:4]),
		binary.BigEndian.Uint32(pkt[4:8]),
		binary.BigEndian.Uint32(pkt[8:12]),
		true
}
