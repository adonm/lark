package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"strings"
	"testing"
	"time"
)

func TestResumeInsideWindowKeepsVoiceOwnershipAndExpiredResnapshots(t *testing.T) {
	b, base := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake"}); err != nil {
		t.Fatal(err)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "vocat-33d8", "cake", capDrag|capWakeWord)

	web := testConnectWS(t, base, "unit", true)
	web.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindWeb, capsMask: capDrag, displayFormats: dispSVG,
		terminalID: "web-11223344", companionID: "cake",
	}))
	welcome := framePayload(web.until(t, hasProto(msgWelcome, "")))
	web.until(t, hasSVG(""))
	var resumeTok [resumeTokenLen]byte
	copy(resumeTok[:], welcome[20:36])
	snapSeq := binary.LittleEndian.Uint32(welcome[16:20])
	if snapSeq == 0 {
		t.Fatal("expected non-zero snapshot seq in WELCOME")
	}

	// Web takes voice ownership via rec_start.
	web.recStart(t, "")
	hw.until(t, func(f wsFrame) bool {
		return hasProto(msgVoiceOwner, "web-11223344")(f)
	})
	web.recEnd(t, "", true)

	// Disconnect web and resume inside the window -> keeps voice ownership and gets fresh snapshot.
	web.conn.Close()
	b.ws.sig.wait(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.terminals["web-11223344"] == nil
	})

	web2 := testConnectWS(t, base, "unit", true)
	web2.sendProto(t, msgResume, encodeTerminalResume(protoVersion1, resumeTok, snapSeq, "web-11223344"))
	w2 := framePayload(web2.until(t, hasProto(msgWelcome, "")))
	newSeq := binary.LittleEndian.Uint32(w2[16:20])
	if newSeq <= snapSeq {
		t.Fatalf("resume welcome seq %d <= prior %d", newSeq, snapSeq)
	}
	web2.until(t, hasSVG(""))
	vo := web2.until(t, hasProto(msgVoiceOwner, "web-11223344"))
	voSeq := binary.LittleEndian.Uint32(framePayload(vo)[0:4])
	if voSeq <= newSeq {
		t.Fatalf("snapshot VOICE_OWNER seq %d <= welcome seq %d", voSeq, newSeq)
	}

	// Expired resume token gets REJECT(rejectUnknownTerminal), then fresh HELLO works.
	web2.conn.Close()
	b.ws.sig.wait(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.terminals["web-11223344"] == nil
	})
	b.ws.mu.Lock()
	for k, e := range b.ws.resumes {
		e.expiresAt = time.Now().Add(-time.Second)
		b.ws.resumes[k] = e
	}
	b.ws.mu.Unlock()

	web3 := testConnectWS(t, base, "unit", true)
	var expiredTok [resumeTokenLen]byte
	copy(expiredTok[:], w2[20:36])
	web3.sendProto(t, msgResume, encodeTerminalResume(protoVersion1, expiredTok, newSeq, "web-11223344"))
	rej := framePayload(web3.until(t, hasProto(msgReject, "")))
	if rej[0] != rejectUnknownTerminal {
		t.Fatalf("expected rejectUnknownTerminal for expired resume, got %d", rej[0])
	}
	web3.helloWeb(t, "web-11223344", "cake", capDrag, false)
}

func TestTwoTerminalsDisplayAdaptationVoiceOwnershipFallbackStatusAndPrompt(t *testing.T) {
	sttGot := make(chan []byte, 1)
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.stt = func(_ context.Context, pcm []byte) (string, error) {
			sttGot <- bytes.Clone(pcm)
			return "", nil
		}
	})
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake"}); err != nil {
		t.Fatal(err)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "vocat-33d8", "cake", capDrag|capWakeWord)

	web := testConnectWS(t, base, "unit", true)
	web.helloWeb(t, "web-aabbccdd", "cake", capDrag, false)

	// 1. Two terminals on one companion: hw gets FRAME, web gets FRAME_SVG.
	toolTest(t, base, "app", object{"companions": "cake", "svg": `<svg><text x="180" y="180">DualView</text></svg>`}, false)
	hw.until(t, isDisplay)
	web.until(t, hasSVG("DualView"))

	// 2. Voice ownership take on tap/rec_start and fallback on disconnect.
	web.recStart(t, "")
	hw.until(t, hasProto(msgVoiceOwner, "web-aabbccdd"))
	web.until(t, hasProto(msgVoiceOwner, "web-aabbccdd"))
	// STATUS broadcast: listening.
	hw.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusListening
	})
	// RTP-in-WS from browser into STT.
	pcm := bytes.Repeat([]byte{0x31, 0x00}, rtpSamples)
	web.sendRTP(t, rtpPTRecPCM, 1, 320, 1, pcm)
	web.recEnd(t, "", false)
	select {
	case got := <-sttGot:
		if !bytes.Equal(got, pcm) {
			t.Fatalf("RTP-in-WS STT mismatch: got %d bytes", len(got))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RTP-in-WS in STT")
	}

	// Disconnect web -> ownership falls back to hw.
	web.conn.Close()
	hw.until(t, hasProto(msgVoiceOwner, "vocat-33d8"))

	// Reconnect web for PROMPT first-reply-wins test.
	web2 := testConnectWS(t, base, "unit", true)
	web2.helloWeb(t, "web-aabbccdd", "cake", capDrag, false)

	// 3. PROMPT broadcast, first PROMPT_REPLY wins, PROMPT_ENDED sent to all.
	replies := make(chan string, 2)
	b.ws.mu.Lock()
	b.ws.openPrompt("cake", promptPermission, "perm-1", "Allow tool?", []string{"allow", "deny"}, func(choice string) {
		replies <- choice
	})
	b.ws.mu.Unlock()

	hw.until(t, hasProto(msgPrompt, "perm-1"))
	web2.until(t, hasProto(msgPrompt, "perm-1"))

	replyPkt := appendStr8(appendStr8(nil, "perm-1"), "allow")
	web2.sendProto(t, msgPromptReply, replyPkt)
	hw.sendProto(t, msgPromptReply, appendStr8(appendStr8(nil, "perm-1"), "deny"))

	hw.until(t, hasProto(msgPromptEnded, "perm-1"))
	web2.until(t, hasProto(msgPromptEnded, "perm-1"))
	select {
	case got := <-replies:
		if got != "allow" {
			t.Fatalf("expected first reply 'allow', got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("prompt callback did not fire")
	}
	select {
	case extra := <-replies:
		t.Fatalf("second reply %q should have been ignored", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProtocolWireSizesAndAllocs(t *testing.T) {
	ptrBytes := len(packProtoPacket(msgPointer, 1, encodePointer(ptrTap, 180, 180, 42)))
	statusBytes := len(packProtoPacket(msgStatus, 2, encodeStatus(1, statusThinking, 50, promptNone, "thinking")))
	if ptrBytes != 16 || statusBytes != 25 {
		t.Fatalf("unexpected wire sizes: pointer=%d status=%d", ptrBytes, statusBytes)
	}

	var enc frameEncoder
	fb := make([]uint16, 360*360)
	rect := image.Rect(0, 0, 360, 360)
	// Warm encoder buffers before measuring steady-state allocations.
	_ = enc.encodeWithCodecs(fb, nil, rect, 1, 360, codecBitAll)
	frameAllocs := testing.AllocsPerRun(20, func() {
		f := enc.encodeWithCodecs(fb, nil, rect, 2, 360, codecBitAll)
		if len(f.data) == 0 {
			t.Fatal("empty frame")
		}
	})

	var pkt [rtpPacketBytes]byte
	pcm := make([]byte, rtpPCMBytes)
	rtpAllocs := testing.AllocsPerRun(100, func() {
		n := encodeRTPPacket(pkt[:], rtpPTRecPCM, 1, 320, 0x1234, pcm)
		if _, _, _, _, ok := decodeRTPHeader(pkt[:n]); !ok {
			t.Fatal("bad rtp")
		}
	})
	if rtpAllocs != 0 {
		t.Fatalf("RTP path allocated %v times/op, want 0", rtpAllocs)
	}
	t.Logf("MEASURE pointer_bytes=%d status_bytes=%d frame_allocs=%.1f rtp_allocs=%.1f",
		ptrBytes, statusBytes, frameAllocs, rtpAllocs)
	if !strings.Contains("ok", "ok") {
		t.Fatal()
	}
}
