package main

import (
	"bytes"
	"context"
	"image"
	"strings"
	"testing"
	"time"
)

func TestResumeInsideWindowKeepsVoiceOwnershipAndExpiredResnapshots(t *testing.T) {
	t.Parallel()
	b, base := wsHarness(t, nil)
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "vocat-33d8", "cake", capDrag)

	hw2 := testConnectWS(t, base, "unit", false)
	hw2.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
		protoVersion: protoVersion1, kind: termKindHardware, capsMask: capDrag, displayFormats: dispRGB565,
		frameCodecs: codecBitAll, width: 360, height: 360, shape: shapeRound,
		terminalID: "vocat-2cac", companionID: "cake", board: "esp-vocat", firmware: "0.12.0", token: "unit",
	}))
	welcome := framePayload(hw2.until(t, hasProto(msgWelcome, "")))
	hw2.until(t, isDisplay)
	var resumeTok [resumeTokenLen]byte
	copy(resumeTok[:], welcome[20:36])

	// Second hardware terminal takes voice ownership via rec_start.
	hw2.recStart(t, "")
	hw.until(t, func(f protocolFrame) bool {
		return hasProto(msgVoiceOwner, "vocat-2cac")(f)
	})
	hw2.recEnd(t, "", true)

	// Disconnect hw2 and resume inside the window -> keeps voice ownership and gets fresh snapshot.
	hw2.conn.Close()
	b.hub.sig.wait(t, func() bool {
		b.hub.mu.Lock()
		defer b.hub.mu.Unlock()
		return b.hub.terminals["vocat-2cac"] == nil
	})

	hw2r := testConnectWS(t, base, "unit", false)
	hw2r.sendProto(t, msgResume, encodeTerminalResume(protoVersion1, resumeTok, "vocat-2cac"))
	hw2r.until(t, hasProto(msgWelcome, ""))
	var gotDisplay, gotVO bool
	for !gotDisplay || !gotVO {
		f := hw2r.read(t)
		if isDisplay(f) {
			gotDisplay = true
		}
		if hasProto(msgVoiceOwner, "vocat-2cac")(f) {
			gotVO = true
		}
	}
	// Expired resume token gets REJECT(rejectUnknownTerminal), then fresh HELLO works.
	hw2r.conn.Close()
	b.hub.sig.wait(t, func() bool {
		b.hub.mu.Lock()
		defer b.hub.mu.Unlock()
		return b.hub.terminals["vocat-2cac"] == nil
	})
	b.hub.mu.Lock()
	for k, e := range b.hub.resumes {
		e.expiresAt = time.Now().Add(-time.Second)
		b.hub.resumes[k] = e
	}
	b.hub.mu.Unlock()

	hw2e := testConnectWS(t, base, "unit", false)
	var expiredTok [resumeTokenLen]byte
	copy(expiredTok[:], resumeTok[:])
	hw2e.sendProto(t, msgResume, encodeTerminalResume(protoVersion1, expiredTok, "vocat-2cac"))
	rej := framePayload(hw2e.until(t, hasProto(msgReject, "")))
	if rej[0] != rejectUnknownTerminal {
		t.Fatalf("expected rejectUnknownTerminal for expired resume, got %d", rej[0])
	}
	hw2f := testConnectWS(t, base, "unit", false)
	hw2f.hello(t, "vocat-2cac", "cake")
}

func TestTwoTerminalsDisplayAdaptationVoiceOwnershipFallbackAndStatus(t *testing.T) {
	t.Parallel()
	sttGot := make(chan []byte, 1)
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.setSTT(func(_ context.Context, pcm []byte) (string, error) {
			sttGot <- bytes.Clone(pcm)
			return "", nil
		})
	})
	if _, err := b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	hw := testConnectWS(t, base, "unit", false)
	hw.helloHardware(t, "vocat-33d8", "cake", capDrag)

	hw2 := testConnectWS(t, base, "unit", false)
	hw2.helloHardware(t, "vocat-2cac", "cake", capDrag)

	// 1. Two terminals on one companion both receive display frames.
	toolTest(t, base, "patch", object{"companion_id": "cake", "companion": object{"name": "Cake Two"}}, false)
	hw.until(t, isDisplay)
	hw2.until(t, isDisplay)

	// 2. Voice ownership take on rec_start and fallback on disconnect.
	hw2.recStart(t, "")
	hw.until(t, hasProto(msgVoiceOwner, "vocat-2cac"))
	hw2.until(t, hasProto(msgVoiceOwner, "vocat-2cac"))
	// STATUS broadcast: listening.
	hw.until(t, func(f protocolFrame) bool {
		p := framePayload(f)
		return hasProto(msgStatus, "")(f) && len(p) >= 5 && p[4] == statusListening
	})
	pcm := bytes.Repeat([]byte{0x31, 0x00}, rtpSamples)
	hw2.sendRTP(t, rtpPTRecPCM, 1, 320, pcm)
	hw2.recEnd(t, "", false)
	select {
	case got := <-sttGot:
		if !bytes.Equal(got, pcm) {
			t.Fatalf("RTP STT mismatch: got %d bytes", len(got))
		}
	case <-time.After(testWait(t)):
		t.Fatal("timed out waiting for RTP in STT")
	}

	// Disconnect hw2 -> ownership falls back to hw.
	hw2.conn.Close()
	hw.until(t, hasProto(msgVoiceOwner, "vocat-33d8"))
}

func TestProtocolWireSizesAndAllocs(t *testing.T) {
	serial(t)
	ptrBytes := len(packProtoPacket(msgPointer, 1, encodePointer(ptrTap, 180, 180, 42)))
	statusBytes := len(packProtoPacket(msgStatus, 2, encodeStatus(statusThinking)))
	if ptrBytes != 16 || statusBytes != 17 {
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
	if frameAllocs > 2 {
		t.Fatalf("frame path allocated %v times/op, want <= 2", frameAllocs)
	}
	t.Logf("MEASURE pointer_bytes=%d status_bytes=%d frame_allocs=%.1f rtp_allocs=%.1f",
		ptrBytes, statusBytes, frameAllocs, rtpAllocs)
	if !strings.Contains("ok", "ok") {
		t.Fatal()
	}
}
