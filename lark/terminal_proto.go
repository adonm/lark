package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
)

const (
	protoMagic      uint8  = 0x4C
	protoHeaderSize        = 8
	protoMaxPayload uint32 = 320 * 1024
	defaultDevPort         = "8766"
	frameTag        byte   = 3

	// Device -> Bridge TCP message types
	msgHello      uint8 = 0x01
	msgStatus     uint8 = 0x02
	msgPointer    uint8 = 0x03
	msgWakeWord   uint8 = 0x04
	msgRecStart   uint8 = 0x05
	msgRecEnd     uint8 = 0x06
	msgTTSAck     uint8 = 0x07
	msgCallFailed uint8 = 0x08
	msgPing       uint8 = 0x09
	msgPong       uint8 = 0x0A

	// Bridge -> Device TCP message types
	msgHelloOK      uint8 = 0x81
	msgFrame        uint8 = 0x82
	msgWakeWordCtrl uint8 = 0x83
	msgRecCtrl      uint8 = 0x84
	msgTTSCtrl      uint8 = 0x85
	msgTTSPCM       uint8 = 0x86
	msgCallCtrl     uint8 = 0x87
	msgChime        uint8 = 0x88
	msgPower        uint8 = 0x89
	msgVolume       uint8 = 0x8A

	// Capability bits
	capBattery        uint32 = 1 << 0
	capHeadPads       uint32 = 1 << 1
	capCall           uint32 = 1 << 2
	capCallPCM        uint32 = 1 << 3
	capDrag           uint32 = 1 << 4
	capPointerRev     uint32 = 1 << 5
	capBridgeEndpoint uint32 = 1 << 6
	capPanelSleep     uint32 = 1 << 7
	capWakeWord       uint32 = 1 << 8

	// Pointer kinds
	ptrTap  uint8 = 0
	ptrDrag uint8 = 1
	ptrUp   uint8 = 2

	// Rec ops & flags
	recOpStop        uint8 = 0
	recOpStart       uint8 = 1
	recOpCancel      uint8 = 2
	recTargetInter   uint8 = 1 << 0
	recTargetBridgeE uint8 = 1 << 7

	// TTS ops
	ttsOpStop  uint8 = 0
	ttsOpStart uint8 = 1
	ttsOpEnd   uint8 = 2

	// Call ops
	callOpEnd    uint8 = 0
	callOpStart  uint8 = 1
	callOpMute   uint8 = 2
	callOpUnmute uint8 = 3

	// Chime kinds
	chimeTap      uint8 = 0
	chimeThink    uint8 = 1
	chimeCancel   uint8 = 2
	chimeIntercom uint8 = 3
	chimeRing     uint8 = 4

	helloFlagBridgeEP uint16 = 1 << 0
	statusFlagSleep   uint8  = 1 << 0

	// RTP constants
	rtpVersionByte uint8  = 0x80
	rtpHeaderSize         = 12
	rtpSamples            = 320
	rtpPCMBytes           = rtpSamples * 2
	rtpPacketBytes        = rtpHeaderSize + rtpPCMBytes
	rtpPTRecPCM    uint8  = 96
	rtpPTCallUp    uint8  = 97
	rtpPTCallDown  uint8  = 98
)

var (
	errProtoMagic = errors.New("invalid protocol magic")
	errProtoSize  = errors.New("protocol payload exceeds limit")
	errProtoShort = errors.New("short protocol payload")
)

type nativeHello struct {
	capsMask uint32
	width    uint16
	height   uint16
	udpPort  uint16
	deviceID string
	board    string
	firmware string
	token    string
}

func packProtoPacket(msgType uint8, seq uint16, payload []byte) []byte {
	pkt := make([]byte, protoHeaderSize+len(payload))
	pkt[0] = protoMagic
	pkt[1] = msgType
	binary.LittleEndian.PutUint16(pkt[2:4], seq)
	binary.LittleEndian.PutUint32(pkt[4:8], uint32(len(payload)))
	copy(pkt[protoHeaderSize:], payload)
	return pkt
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

func parseNativeHello(b []byte) (nativeHello, error) {
	if len(b) < 14 {
		return nativeHello{}, errProtoShort
	}
	h := nativeHello{
		capsMask: binary.LittleEndian.Uint32(b[0:4]),
		width:    binary.LittleEndian.Uint16(b[4:6]),
		height:   binary.LittleEndian.Uint16(b[6:8]),
		udpPort:  binary.LittleEndian.Uint16(b[8:10]),
	}
	idLen, boardLen, fwLen, tokLen := int(b[10]), int(b[11]), int(b[12]), int(b[13])
	need := 14 + idLen + boardLen + fwLen + tokLen
	if len(b) < need {
		return nativeHello{}, errProtoShort
	}
	off := 14
	h.deviceID = string(b[off : off+idLen])
	off += idLen
	h.board = string(b[off : off+boardLen])
	off += boardLen
	h.firmware = string(b[off : off+fwLen])
	off += fwLen
	h.token = string(b[off : off+tokLen])
	return h, nil
}

func helloCapsFromMask(mask uint32) terminalCaps {
	call := mask&capCall != 0
	return terminalCaps{
		Battery:        mask&capBattery != 0,
		HeadPads:       mask&capHeadPads != 0,
		Call:           call,
		CallPCM:        call && mask&capCallPCM != 0,
		Drag:           mask&capDrag != 0,
		PointerRev:     mask&capPointerRev != 0,
		BridgeEndpoint: mask&capBridgeEndpoint != 0,
		PanelSleep:     mask&capPanelSleep != 0,
		WakeWord:       mask&capWakeWord != 0,
	}
}

func encodeHelloOK(udpPort, flags uint16, ssrc, sessionID uint32) []byte {
	var b [12]byte
	binary.LittleEndian.PutUint16(b[0:2], udpPort)
	binary.LittleEndian.PutUint16(b[2:4], flags)
	binary.LittleEndian.PutUint32(b[4:8], ssrc)
	binary.LittleEndian.PutUint32(b[8:12], sessionID)
	return b[:]
}

func parseNativeStatus(b []byte) (wsStatus, bool) {
	if len(b) < 16 {
		return wsStatus{}, false
	}
	rssi := int(int16(binary.LittleEndian.Uint16(b[0:2])))
	batt := int(int16(binary.LittleEndian.Uint16(b[2:4])))
	charging := b[4] != 0
	cpu := float64(b[5])
	freeHeap := uint64(binary.LittleEndian.Uint32(b[8:12]))
	uptime := uint64(binary.LittleEndian.Uint32(b[12:16]))
	var battPtr *int
	if batt >= 0 {
		battPtr = new(batt)
	}
	return wsStatus{
		WiFi:     new(rssi),
		Battery:  battPtr,
		Charge:   new(charging),
		CPU:      new(math.Min(100, cpu)),
		FreeHeap: new(freeHeap),
		Uptime:   new(uptime),
	}, true
}

func parseNativePointer(b []byte) (evName string, x, y int, rev uint16, ok bool) {
	if len(b) < 8 {
		return "", 0, 0, 0, false
	}
	switch b[0] {
	case ptrTap:
		evName = "tap"
	case ptrDrag:
		evName = "drag"
	case ptrUp:
		evName = "up"
	default:
		return "", 0, 0, 0, false
	}
	x = int(binary.LittleEndian.Uint16(b[2:4]))
	y = int(binary.LittleEndian.Uint16(b[4:6]))
	rev = binary.LittleEndian.Uint16(b[6:8])
	return evName, x, y, rev, true
}

func parseNativeRecStart(b []byte) (session uint16, target uint8, ok bool) {
	if len(b) < 4 {
		return 0, 0, false
	}
	return binary.LittleEndian.Uint16(b[0:2]), b[2], true
}

func parseNativeRecEnd(b []byte) (session uint16, target uint8, cancelled bool, ok bool) {
	if len(b) < 4 {
		return 0, 0, false, false
	}
	return binary.LittleEndian.Uint16(b[0:2]), b[2], b[3] != 0, true
}

func parseNativeTTSAck(b []byte) (aborted bool, audioID uint64, ok bool) {
	if len(b) < 2 {
		return false, 0, false
	}
	idLen := int(b[1])
	if len(b) < 2+idLen || idLen == 0 {
		return false, 0, false
	}
	id, err := strconv.ParseUint(string(b[2:2+idLen]), 10, 64)
	if err != nil {
		return false, 0, false
	}
	return b[0] != 0, id, true
}

func parseNativeCallFailed(b []byte) (callID uint32, ok bool) {
	if len(b) < 1 {
		return 0, false
	}
	idLen := int(b[0])
	if len(b) < 1+idLen || idLen == 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(string(b[1:1+idLen]), 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(id), true
}

func encodeRecCtrl(op, target uint8, session uint16) []byte {
	var b [4]byte
	b[0] = op
	b[1] = target
	binary.LittleEndian.PutUint16(b[2:4], session)
	return b[:]
}

func encodeIDCtrl(op uint8, id uint64) []byte {
	s := strconv.FormatUint(id, 10)
	b := make([]byte, 2+len(s))
	b[0] = op
	b[1] = uint8(len(s))
	copy(b[2:], s)
	return b
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

func encodePowerPayload(brightness int) byte {
	switch brightness {
	case 85:
		return 0
	case 12:
		return 1
	case 20:
		return 2
	default:
		if brightness < 12 {
			return 1
		}
		if brightness > 85 {
			return 85
		}
		return byte(brightness)
	}
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
