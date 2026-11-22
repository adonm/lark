package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const (
	protoMagic      uint8  = 0x4C
	protoVersion1   uint8  = 1
	protoHeaderSize        = 8
	protoMaxPayload uint32 = 320 * 1024
	resumeTokenLen         = 16
	resumeWindowMS  uint32 = 30000
	defaultDevPort         = "8766"
	frameTag        byte   = 3

	// Terminal -> Lark message types
	msgHello       uint8 = 0x01
	msgResume      uint8 = 0x02
	msgTelemetry   uint8 = 0x03
	msgPointer     uint8 = 0x04
	msgWakeWord    uint8 = 0x05
	msgRecStart    uint8 = 0x06
	msgRecEnd      uint8 = 0x07
	msgTTSAck      uint8 = 0x08
	msgCallReq     uint8 = 0x09
	msgMedia       uint8 = 0x0A
	msgTextInput   uint8 = 0x0B
	msgPromptReply uint8 = 0x0C
	msgWatch       uint8 = 0x0D
	msgSetVolume   uint8 = 0x0E
	msgPing        uint8 = 0x0F
	msgPong        uint8 = 0x10

	// Lark -> Terminal message types
	msgWelcome      uint8 = 0x81
	msgReject       uint8 = 0x82
	msgFrame        uint8 = 0x83
	msgFrameSVG     uint8 = 0x84
	msgWakeWordCtrl uint8 = 0x85
	msgRecCtrl      uint8 = 0x86
	msgTTSCtrl      uint8 = 0x87
	msgTTSPCM       uint8 = 0x88
	msgCallCtrl     uint8 = 0x89
	msgChime        uint8 = 0x8A
	msgPower        uint8 = 0x8B
	msgVolume       uint8 = 0x8C
	msgStatus       uint8 = 0x8D
	msgVoiceOwner   uint8 = 0x8E
	msgPrompt       uint8 = 0x8F
	msgPromptEnded  uint8 = 0x90
	msgCompanion    uint8 = 0x91
	msgPeers        uint8 = 0x92
	msgEvent        uint8 = 0x93

	// Terminal kind
	termKindHardware uint8 = 0
	termKindWeb      uint8 = 1

	// Display formats
	dispRGB565 uint8 = 1 << 0
	dispSVG    uint8 = 1 << 1

	// Frame codecs
	codecBitRaw    uint8 = 1 << 0
	codecBitRLE565 uint8 = 1 << 1
	codecBitLZ4    uint8 = 1 << 2
	codecBitJPEG   uint8 = 1 << 3
	codecBitAll    uint8 = codecBitRaw | codecBitRLE565 | codecBitLZ4 | codecBitJPEG

	// Screen shape
	shapeRound uint8 = 0
	shapeRect  uint8 = 1

	// Hello flags
	helloFlagIfFree uint8 = 1 << 0

	// Welcome flags
	welcomeFlagBridgeEP uint8 = 1 << 0

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

	// Reject reasons
	rejectBadToken        uint8 = 1
	rejectBadVersion      uint8 = 2
	rejectUnknownTerminal uint8 = 3
	rejectFull            uint8 = 4
	rejectOccupied        uint8 = 5
	rejectBadCaps         uint8 = 6

	// Status states
	statusIdle      uint8 = 0
	statusListening uint8 = 1
	statusThinking  uint8 = 2
	statusSpeaking  uint8 = 3
	statusBlocked   uint8 = 4
	statusDone      uint8 = 5
	statusError     uint8 = 6

	// Prompt / status kinds
	promptNone       uint8 = 0
	promptPermission uint8 = 1
	promptQuestion   uint8 = 2

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

	// Event kinds
	eventSavedApps       uint8 = 1
	eventTimers          uint8 = 2
	eventChat            uint8 = 3
	eventHistoryArchived uint8 = 4
	eventParentPolicy    uint8 = 5

	// RTP constants
	rtpVersionByte uint8 = 0x80
	rtpHeaderSize        = 12
	rtpSamples           = 320
	rtpPCMBytes          = rtpSamples * 2
	rtpPacketBytes       = rtpHeaderSize + rtpPCMBytes
	rtpPTRecPCM    uint8 = 96
	rtpPTCallUp    uint8 = 97
	rtpPTCallDown  uint8 = 98
)

var (
	errProtoMagic = errors.New("invalid protocol magic")
	errProtoSize  = errors.New("protocol payload exceeds limit")
	errProtoShort = errors.New("short protocol payload")
)

type terminalHello struct {
	protoVersion   uint8
	kind           uint8
	capsMask       uint32
	displayFormats uint8
	frameCodecs    uint8
	width          uint16
	height         uint16
	shape          uint8
	flags          uint8
	udpPort        uint16
	terminalID     string
	companionID    string
	board          string
	firmware       string
	token          string
}

type terminalResume struct {
	protoVersion uint8
	resumeToken  [resumeTokenLen]byte
	lastSeq      uint32
	terminalID   string
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
	if len(b) < 21 {
		return terminalHello{}, errProtoShort
	}
	h := terminalHello{
		protoVersion:   b[0],
		kind:           b[1],
		capsMask:       binary.LittleEndian.Uint32(b[2:6]),
		displayFormats: b[6],
		frameCodecs:    b[7],
		width:          binary.LittleEndian.Uint16(b[8:10]),
		height:         binary.LittleEndian.Uint16(b[10:12]),
		shape:          b[12],
		flags:          b[13],
		udpPort:        binary.LittleEndian.Uint16(b[14:16]),
	}
	idLen, compLen, boardLen, fwLen, tokLen := int(b[16]), int(b[17]), int(b[18]), int(b[19]), int(b[20])
	if len(b) < 21+idLen+compLen+boardLen+fwLen+tokLen {
		return terminalHello{}, errProtoShort
	}
	off := 21
	h.terminalID = string(b[off : off+idLen])
	off += idLen
	h.companionID = string(b[off : off+compLen])
	off += compLen
	h.board = string(b[off : off+boardLen])
	off += boardLen
	h.firmware = string(b[off : off+fwLen])
	off += fwLen
	h.token = string(b[off : off+tokLen])
	return h, nil
}

func encodeTerminalHello(h terminalHello) []byte {
	tidLen := min(len(h.terminalID), 255)
	cidLen := min(len(h.companionID), 255)
	bdLen := min(len(h.board), 255)
	fwLen := min(len(h.firmware), 255)
	tokLen := min(len(h.token), 255)
	b := make([]byte, 0, 21+tidLen+cidLen+bdLen+fwLen+tokLen)
	b = append(b, h.protoVersion, h.kind)
	b = binary.LittleEndian.AppendUint32(b, h.capsMask)
	b = append(b, h.displayFormats, h.frameCodecs)
	b = binary.LittleEndian.AppendUint16(b, h.width)
	b = binary.LittleEndian.AppendUint16(b, h.height)
	b = append(b, h.shape, h.flags)
	b = binary.LittleEndian.AppendUint16(b, h.udpPort)
	b = append(b, byte(tidLen), byte(cidLen), byte(bdLen), byte(fwLen), byte(tokLen))
	b = append(b, h.terminalID[:tidLen]...)
	b = append(b, h.companionID[:cidLen]...)
	b = append(b, h.board[:bdLen]...)
	b = append(b, h.firmware[:fwLen]...)
	b = append(b, h.token[:tokLen]...)
	return b
}

func parseTerminalResume(b []byte) (terminalResume, error) {
	if len(b) < 22 {
		return terminalResume{}, errProtoShort
	}
	var r terminalResume
	r.protoVersion = b[0]
	copy(r.resumeToken[:], b[1:17])
	r.lastSeq = binary.LittleEndian.Uint32(b[17:21])
	idLen := int(b[21])
	if len(b) < 22+idLen {
		return terminalResume{}, errProtoShort
	}
	r.terminalID = string(b[22 : 22+idLen])
	return r, nil
}

func encodeTerminalResume(protoVer uint8, token [resumeTokenLen]byte, lastSeq uint32, terminalID string) []byte {
	b := make([]byte, 0, 22+len(terminalID))
	b = append(b, protoVer)
	b = append(b, token[:]...)
	b = binary.LittleEndian.AppendUint32(b, lastSeq)
	return appendStr8(b, terminalID)
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

func capsMaskFromCaps(c terminalCaps) uint32 {
	var m uint32
	if c.Battery {
		m |= capBattery
	}
	if c.HeadPads {
		m |= capHeadPads
	}
	if c.Call {
		m |= capCall
	}
	if c.CallPCM {
		m |= capCallPCM
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
	if c.WakeWord {
		m |= capWakeWord
	}
	return m
}

func encodeWelcome(flags uint8, udpPort uint16, ssrc, maxMsgBytes, resumeWinMS, seq uint32, token [resumeTokenLen]byte, terminalID, companionID string) []byte {
	b := make([]byte, 0, 38+len(terminalID)+len(companionID))
	b = append(b, protoVersion1, flags)
	b = binary.LittleEndian.AppendUint16(b, udpPort)
	b = binary.LittleEndian.AppendUint32(b, ssrc)
	b = binary.LittleEndian.AppendUint32(b, maxMsgBytes)
	b = binary.LittleEndian.AppendUint32(b, resumeWinMS)
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = append(b, token[:]...)
	tidLen := min(len(terminalID), 255)
	cidLen := min(len(companionID), 255)
	b = append(b, byte(tidLen), byte(cidLen))
	b = append(b, terminalID[:tidLen]...)
	b = append(b, companionID[:cidLen]...)
	return b
}

func encodeReject(reason uint8, message string) []byte {
	b := make([]byte, 0, 2+len(message))
	b = append(b, reason)
	return appendStr8(b, message)
}

func parseTelemetry(b []byte) (wsStatus, bool) {
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

func parsePointer(b []byte) (evName string, x, y int, rev uint16, ok bool) {
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

func encodePointer(kind uint8, x, y, revLo uint16) []byte {
	var b [8]byte
	b[0] = kind
	binary.LittleEndian.PutUint16(b[2:4], x)
	binary.LittleEndian.PutUint16(b[4:6], y)
	binary.LittleEndian.PutUint16(b[6:8], revLo)
	return b[:]
}

func parseRecStart(b []byte) (session uint16, target string, ok bool) {
	if len(b) < 3 {
		return 0, "", false
	}
	off := 2
	target, ok = readStr8(b, &off)
	return binary.LittleEndian.Uint16(b[0:2]), target, ok
}

func parseRecEnd(b []byte) (session uint16, cancelled bool, target string, ok bool) {
	if len(b) < 4 {
		return 0, false, "", false
	}
	off := 3
	target, ok = readStr8(b, &off)
	return binary.LittleEndian.Uint16(b[0:2]), b[2] != 0, target, ok
}

func parseTTSAck(b []byte) (aborted bool, audioID uint64, ok bool) {
	if len(b) < 9 {
		return false, 0, false
	}
	return b[0] != 0, binary.LittleEndian.Uint64(b[1:9]), true
}

func encodeTTSAck(aborted bool, audioID uint64) []byte {
	var b [9]byte
	if aborted {
		b[0] = 1
	}
	binary.LittleEndian.PutUint64(b[1:9], audioID)
	return b[:]
}

func parseCallReq(b []byte) (action uint8, callID uint32, target string, ok bool) {
	if len(b) < 6 {
		return 0, 0, "", false
	}
	off := 5
	target, ok = readStr8(b, &off)
	return b[0], binary.LittleEndian.Uint32(b[1:5]), target, ok
}

func parseTextInput(b []byte) (string, bool) {
	off := 0
	return readStr16(b, &off)
}

func parsePromptReply(b []byte) (id, choice string, ok bool) {
	off := 0
	id, ok = readStr8(b, &off)
	if !ok {
		return "", "", false
	}
	choice, ok = readStr8(b, &off)
	return id, choice, ok
}

func parseWatch(b []byte) (string, bool) {
	off := 0
	return readStr8(b, &off)
}

func parseSetVolume(b []byte) (auto bool, level int, ok bool) {
	if len(b) < 2 {
		return false, 0, false
	}
	return b[0] != 0, int(b[1]), true
}

func encodeFrameSVG(rev uint32, terminalID, companionID, svg string) []byte {
	tidLen := min(len(terminalID), 255)
	cidLen := min(len(companionID), 255)
	b := make([]byte, 0, 10+tidLen+cidLen+len(svg))
	b = binary.LittleEndian.AppendUint32(b, rev)
	b = append(b, byte(tidLen), byte(cidLen))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(svg)))
	b = append(b, terminalID[:tidLen]...)
	b = append(b, companionID[:cidLen]...)
	return append(b, svg...)
}

func encodeWakeWordCtrl(enabled bool, terminalID, companionID string) []byte {
	var en byte
	if enabled {
		en = 1
	}
	b := make([]byte, 0, 3+len(terminalID)+len(companionID))
	b = append(b, en)
	b = appendStr8(b, terminalID)
	return appendStr8(b, companionID)
}

func encodeRecCtrl(op, flags uint8, session uint16, target string) []byte {
	b := make([]byte, 0, 5+len(target))
	b = append(b, op, flags)
	b = binary.LittleEndian.AppendUint16(b, session)
	return appendStr8(b, target)
}

func encodeTTSCtrl(op uint8, audioID uint64) []byte {
	var b [9]byte
	b[0] = op
	binary.LittleEndian.PutUint64(b[1:9], audioID)
	return b[:]
}

func encodeCallCtrl(op uint8, callID uint32, reason string) []byte {
	b := make([]byte, 0, 6+len(reason))
	b = append(b, op)
	b = binary.LittleEndian.AppendUint32(b, callID)
	return appendStr8(b, reason)
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
	var p, a byte
	if persist {
		p = 1
	}
	if auto {
		a = 1
	}
	b := make([]byte, 0, 5+len(terminalID)+len(companionID))
	b = append(b, byte(min(100, max(0, level))), p, a)
	b = appendStr8(b, terminalID)
	return appendStr8(b, companionID)
}

func encodeStatus(seq uint32, state, progress, kind uint8, text string) []byte {
	b := make([]byte, 0, 9+len(text))
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = append(b, state, progress, kind)
	return appendStr16(b, text)
}

func encodeVoiceOwner(seq uint32, companionID, terminalID string) []byte {
	b := make([]byte, 0, 6+len(companionID)+len(terminalID))
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = appendStr8(b, companionID)
	return appendStr8(b, terminalID)
}

func encodePrompt(seq uint32, kind uint8, id, text string, options []string) []byte {
	b := make([]byte, 0, 16+len(id)+len(text))
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = append(b, kind)
	b = appendStr8(b, id)
	b = appendStr16(b, text)
	n := min(len(options), 255)
	b = append(b, byte(n))
	for i := range n {
		b = appendStr8(b, options[i])
	}
	return b
}

func encodePromptEnded(seq uint32, id string) []byte {
	b := make([]byte, 0, 5+len(id))
	b = binary.LittleEndian.AppendUint32(b, seq)
	return appendStr8(b, id)
}

func encodeCompanionMsg(seq uint32, id, name, voice, faceBG, markColor, accentColor string) []byte {
	b := make([]byte, 0, 16+len(id)+len(name)+len(voice)+24)
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = appendStr8(b, id)
	b = appendStr8(b, name)
	b = appendStr8(b, voice)
	b = appendStr8(b, faceBG)
	b = appendStr8(b, markColor)
	return appendStr8(b, accentColor)
}

func encodeEventMsg(seq uint32, kind uint8, companionID, arg1, arg2 string) []byte {
	b := make([]byte, 0, 9+len(companionID)+len(arg1)+len(arg2))
	b = binary.LittleEndian.AppendUint32(b, seq)
	b = append(b, kind)
	b = appendStr8(b, companionID)
	b = appendStr8(b, arg1)
	return appendStr16(b, arg2)
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
