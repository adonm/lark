//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const e2eVoiceQuestion = "Why is the sky blue? Answer in one short sentence."

func TestVirtualE2E(t *testing.T) {
	switch mode := os.Getenv("LARK_E2E_MODE"); mode {
	case "stub", "real":
		s := startE2E(t, mode == "real")
		t.Run("IdleWake", func(t *testing.T) {
			t.Parallel()
			c := s.board(t, "esp-vocat", "Idle")
			s.idleWake(t, c, time.Now())
		})
		t.Run("CallTimeout", func(t *testing.T) {
			t.Parallel()
			s.callTimeout(t)
		})
		t.Run("Product", func(t *testing.T) {
			t.Parallel() // Keep idle/call sockets reading throughout the real timeouts.
			t.Run("VoiceTypedAndStop", s.voiceTypedStop)
			t.Run("GradedEndpointAndQueuedVoice", s.gradedEndpointAndQueuedVoice)
			t.Run("VoCatMenus", func(t *testing.T) { s.menus(t, "esp-vocat", "MenusRound") })
			t.Run("Box3Menus", func(t *testing.T) { s.menus(t, "esp-box-3", "MenusRect") })
			t.Run("RawFramebuffer", s.rawFramebuffer)
			t.Run("IntercomReply", s.intercom)
			t.Run("Calls", s.calls)
			t.Run("Apps", s.apps)
			t.Run("TimersMemoryHistory", s.data)
			t.Run("ScopedTunnel", s.scopedTunnel)
			t.Run("Diagnostics", s.diagnostics)
		})
	default:
		t.Skip("use scripts/check.sh --live or --e2e-real")
	}
}

func e2eFields(f wsFrame, op string, pairs ...string) bool {
	if f.op != wsBinary {
		return false
	}
	mt, _, p, ok := unpackProtoPacket(f.data)
	if !ok {
		return false
	}
	switch op {
	case "chime":
		if mt != msgChime || len(p) < 1 {
			return false
		}
		if len(pairs) >= 2 && pairs[0] == "kind" {
			want := chimeTap
			switch strings.Trim(pairs[1], `"`) {
			case "cancel":
				want = chimeCancel
			case "ring":
				want = chimeRing
			case "intercom":
				want = chimeIntercom
			case "think":
				want = chimeThink
			}
			return p[0] == want
		}
		return true
	case "rec":
		if mt != msgRecCtrl || len(p) < 5 {
			return false
		}
		for i := 0; i < len(pairs); i += 2 {
			if pairs[i] == "action" && strings.Trim(pairs[i+1], `"`) == "start" && p[0] != recOpStart {
				return false
			}
			if pairs[i] == "action" && strings.Trim(pairs[i+1], `"`) == "stop" && p[0] != recOpStop && p[0] != recOpCancel {
				return false
			}
			if pairs[i] == "cancelled" && pairs[i+1] == "true" && p[0] != recOpCancel {
				return false
			}
			if pairs[i] == "endpoint" && p[1]&recTargetBridgeE == 0 {
				return false
			}
		}
		return true
	case "tts":
		if mt != msgTTSCtrl || len(p) < 9 {
			return false
		}
		for i := 0; i < len(pairs); i += 2 {
			if pairs[i] == "state" {
				st := strings.Trim(pairs[i+1], `"`)
				if st == "start" && p[0] != ttsOpStart || st == "end" && p[0] != ttsOpEnd || st == "abort" && p[0] != ttsOpStop {
					return false
				}
			}
			if pairs[i] == "audio" && fmt.Sprint(binary.LittleEndian.Uint64(p[1:9])) != pairs[i+1] {
				return false
			}
		}
		return true
	case "call":
		if mt != msgCallCtrl || len(p) < 6 {
			return false
		}
		for i := 0; i < len(pairs); i += 2 {
			if pairs[i] == "state" {
				st := strings.Trim(pairs[i+1], `"`)
				if st == "start" && p[0] != callOpStart || st == "end" && p[0] != callOpEnd {
					return false
				}
			}
			if pairs[i] == "reason" && !bytes.Contains(p[6:], []byte(strings.Trim(pairs[i+1], `"`))) {
				return false
			}
		}
		return true
	case "capabilities":
		return mt == msgWelcome && len(p) >= 2 && p[1]&1 != 0
	case "error":
		return mt == msgReject
	}
	return false
}

func (c *e2eCompanion) op(t *testing.T, op string, pairs ...string) wsFrame {
	t.Helper()
	return c.expect(t, op+" "+strings.Join(pairs, " "), func(f wsFrame) bool { return e2eFields(f, op, pairs...) })
}

func (c *e2eCompanion) tapScene(t *testing.T, id, want string) string {
	t.Helper()
	c.tap(t, id)
	return c.scene(t, want)
}

func (c *e2eCompanion) note(t *testing.T, pcm []byte, target string) time.Time {
	t.Helper()
	c.recStart(t, target)
	c.audio(t, 0x01, pcm)
	start := time.Now()
	c.recEnd(t, "", false)
	return start
}

func (c *e2eCompanion) playback(t *testing.T, start time.Time) []byte {
	t.Helper()
	pcm, _ := c.measuredPlayback(t, start)
	return pcm
}

func (c *e2eCompanion) measuredPlayback(t *testing.T, start time.Time) ([]byte, time.Time) {
	t.Helper()
	var first time.Time
	var pcm []byte
	var audio int
	end := c.expect(t, "complete TTS playback", func(f wsFrame) bool {
		switch {
		case e2eFields(f, "tts", "state", `"start"`):
			if audio != 0 {
				t.Fatal("overlapping TTS playback")
			}
			audio = e2eNumber(testObject(t, f.data)["audio"])
		case isPlayback(f):
			if audio <= 0 {
				t.Fatal("PCM arrived before a numbered TTS start")
			}
			if first.IsZero() {
				first = time.Now()
			}
			pcm = append(pcm, f.data[1:]...)
		}
		return e2eFields(f, "tts", "state", `"end"`)
	})
	if !livePCMOK(pcm) || e2eNumber(testObject(t, end.data)["audio"]) != audio {
		t.Fatal("playback did not finish with matching audio ID and non-silent samples")
	}
	t.Logf("input end → first PCM %s (%d ms total audio)", first.Sub(start).Round(time.Millisecond), len(pcm)/(sampleRate*2/1000))
	c.ack(t, end)
	return pcm, first
}

func (c *e2eCompanion) capture(t *testing.T, pcm []byte) time.Time {
	t.Helper()
	c.recStart(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		const tick = 40 * time.Millisecond
		const size = rtpPCMBytes
		mic := append(slices.Clone(pcm), make([]byte, size*150)...)
		var seq uint16
		for offset := 0; offset+size <= len(mic); offset += size {
			timer := time.NewTimer(time.Until(start.Add(time.Duration(offset/size+1) * tick)))
			select {
			case <-ctx.Done():
				timer.Stop()
				done <- nil
				return
			case <-timer.C:
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
			var rtp [rtpPacketBytes]byte
			seq++
			n := encodeRTPPacket(rtp[:], rtpPTRecPCM, seq, uint32(seq)*rtpSamples, 1, mic[offset:offset+size])
			_, err := c.conn.Write(maskedFrame(wsBinary, true, packProtoPacket(msgMedia, 0, rtp[:n])))
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	stop := c.op(t, "rec", "action", `"stop"`)
	if framePayload(stop)[0] == recOpCancel {
		t.Fatal("Silero discarded the speech fixture")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal("paced microphone write failed:", err)
	}
	end := start.Add(pcmDuration(len(pcm)))
	if time.Now().Before(end) {
		t.Fatal("bridge stopped before the entire fixture was spoken")
	}
	c.recEnd(t, "", false)
	return end
}

func e2eEndpointWait(t *testing.T, turn diagTurn, want time.Duration) {
	t.Helper()
	if turn.STT == nil || turn.Endpoint == nil {
		t.Fatal("missing native STT or endpoint timings")
	}
	wait := time.Duration(*turn.Endpoint * float64(time.Millisecond))
	if wait < want-40*time.Millisecond || wait > want+260*time.Millisecond {
		t.Fatalf("endpoint wait %s, want %s", wait, want)
	}
}

func (s *e2eSuite) turnStages(t *testing.T, c *e2eCompanion, first time.Time) diagTurn {
	t.Helper()
	var report struct {
		Turns []diagTurn `json:"recent_turns"`
	}
	if err := json.Unmarshal(s.request(t, "GET", "/api/diagnostics", nil, 200), &report); err != nil {
		t.Fatal("cannot decode turn timings")
	}
	for _, r := range report.Turns {
		if r.Terminal != c.id {
			continue
		}
		if r.FirstPCM == nil || r.Gate == nil || r.PromptPrep == nil || r.PromptSubmit == nil ||
			r.FirstSentence == nil || r.TTS == nil || r.TTSQueue == nil || r.TTSInfer == nil {
			t.Fatal("missing prompt or playback timing")
		}
		asMS := func(v *float64) string {
			if v == nil {
				return "-"
			}
			return fmt.Sprintf("%.2f", *v)
		}
		ws := milliseconds(first.Sub(time.UnixMilli(r.TS))) - *r.FirstPCM
		t.Logf("stages (ms): endpoint=%s STT=%s gate=%.2f prepare=%.2f submit=%.2f first_text=%s first_sentence=%.2f Kokoro_queue=%.2f infer=%.2f TTS_first=%.2f turn_to_WS_enqueue=%.2f enqueue_to_receive≈%.2f",
			asMS(r.Endpoint), asMS(r.STT), *r.Gate, *r.PromptPrep, *r.PromptSubmit, asMS(r.First),
			*r.FirstSentence, *r.TTSQueue, *r.TTSInfer, *r.TTS, *r.FirstPCM, ws)
		return r
	}
	t.Fatal("missing device turn timing")
	return diagTurn{}
}

func (s *e2eSuite) voiceTypedStop(t *testing.T) {
	t.Run("Voice", func(t *testing.T) {
		c := s.board(t, "esp-vocat", "Voice")
		c.timeout = 40 * time.Second
		c.mirrorOn(t, s)
		pcm := s.synth(t, e2eVoiceQuestion)
		c.tap(t, "companion-ring")
		c.op(t, "rec", "action", `"start"`, "endpoint", `"bridge"`)
		_, first := c.measuredPlayback(t, c.capture(t, pcm))
		e2eEndpointWait(t, s.turnStages(t, c, first), tunedEndpoint.complete)
		rows := s.history(t, c.id, "")
		if len(rows) != 2 || rows[0].Source != "voice" || !strings.Contains(strings.ToLower(rows[0].Text), "sky blue") ||
			rows[1].Role != "companion" || strings.TrimSpace(rows[1].Text) == "" {
			t.Fatal("voice did not persist a real transcript and assistant reply")
		}
		if !s.real && rows[1].Text != e2eModelReply {
			t.Fatal("unexpected deterministic voice reply")
		}
		t.Logf("Parakeet transcript: %s", rows[0].Text)
	})
	t.Run("Typed", func(t *testing.T) {
		c := s.browser(t, "Typed")
		c.timeout = 40 * time.Second
		start := time.Now()
		c.text(t, "Say hello in one short sentence, without tools.")
		_, first := c.measuredPlayback(t, start)
		stage := s.turnStages(t, c, first)
		if !s.real {
			if stage.First == nil {
				t.Fatal("stub first text did not traverse the event feed")
			}
			if d := first.Sub(start); d >= time.Second {
				t.Errorf("stub typed turn took %s to reach first PCM; want under 1 s", d.Round(time.Millisecond))
			}
		}
		rows := s.history(t, c.id, "")
		if len(rows) != 2 || rows[0].Source != "typed" || !strings.HasPrefix(rows[0].Text, "Say hello") || rows[1].Role != "companion" {
			t.Fatal("typed /app turn did not write both canonical messages")
		}
		t.Run("PhoneIdleReconnect", func(t *testing.T) { s.phoneIdleReconnect(t, c, rows) })
	})
	t.Run("ThinkingStopAndStaleTap", func(t *testing.T) {
		if s.real {
			t.Skip("the deterministic model hold is exercised in --live")
		}
		c := s.board(t, "esp-vocat", "StopThink")
		c.mirrorOn(t, s)
		c.text(t, "Virtual test hold")
		c.scene(t, `aria-label="Stop"`)
		c.input(t, "tap", 180, 160, c.revision)
		c.input(t, "tap", 180, 315, c.revision-1)
		c.write(t, maskedFrame(wsPing, true, []byte("pre-stop")))
		c.expect(t, "face/stale taps ignored", func(f wsFrame) bool {
			if e2eFields(f, "chime", "kind", `"tap"`) || e2eFields(f, "chime", "kind", `"cancel"`) || hasProto(msgRecCtrl, "")(f) {
				t.Fatal("a face or stale Stop tap acted on the thinking turn")
			}
			return f.op == wsPong && string(f.data) == "pre-stop"
		})
		c.tapScene(t, "stop", "Tap 2×")
		c.tapScene(t, "stop", "Tap 1×")
		c.tap(t, "stop")
		c.op(t, "chime", "kind", `"cancel"`)
		c.scene(t, `data-id="menu"`)
		if rows := s.history(t, c.id, ""); len(rows) != 1 || rows[0].Role != "user" {
			t.Fatal("cancelled thinking turn wrote an assistant reply")
		}
	})
	t.Run("SpeakingStop", func(t *testing.T) {
		c := s.board(t, "esp-box-3", "StopSpeak")
		c.mirrorOn(t, s)
		c.timeout = 40 * time.Second
		c.text(t, "Say two short sentences about cats, without tools.")
		start := c.op(t, "tts", "state", `"start"`)
		c.expect(t, "spoken PCM", isPlayback)
		c.sceneWhere(t, "speaking Stop", func(f wsFrame) bool {
			return bytes.Contains(f.data, []byte(`aria-label="Stop"`)) && !bytes.Contains(f.data, []byte("Thinking"))
		})
		c.input(t, "tap", 180, 160, c.revision)
		c.write(t, maskedFrame(wsPing, true, []byte("face-speaking")))
		c.expect(t, "face tap does not act while speaking", func(f wsFrame) bool {
			if e2eFields(f, "tts", "state", `"abort"`) || e2eFields(f, "rec", "action", `"start"`) {
				t.Fatal("face tap interrupted speaking")
			}
			return f.op == wsPong && string(f.data) == "face-speaking"
		})
		c.tap(t, "stop")
		audio := fmt.Sprint(binary.LittleEndian.Uint64(framePayload(start)[1:9]))
		c.op(t, "tts", "state", `"abort"`, "audio", audio)
		c.scene(t, `data-id="menu"`)
	})
}

func (s *e2eSuite) phoneIdleReconnect(t *testing.T, c *e2eCompanion, rows []chatEntry) {
	watcher := testConnectWS(t, s.base, s.token, true)
	watcher.sendProto(t, msgHello, encodeTerminalHello(terminalHello{protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG}))
	watcher.until(t, hasProto(msgPeers, c.id))
	join := func() *testWS {
		ws := testConnectWS(t, s.base, s.token, true)
		ws.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
			protoVersion: protoVersion1, kind: termKindWeb, capsMask: capBridgeEndpoint | capDrag,
			displayFormats: dispSVG, flags: helloFlagIfFree, terminalID: c.id, companionID: c.id,
		}))
		return ws
	}
	closeOp := func(f wsFrame) bool { return f.op == wsClose }
	if f := join().until(t, closeOp); !bytes.Equal(f.data, wsCloseFrame(wsCompanionOccupied).data) {
		t.Fatal("tap-to-reconnect displaced a connected cat")
	}
	// idle.js unit tests exercise the hidden/inactive clocks. Emulate the
	// browser's normal idle close and explicit IfFree hello on the live wire.
	close := wsCloseFrame(1000)
	c.write(t, maskedFrame(wsClose, true, close.data))
	if f := c.until(t, closeOp); !bytes.Equal(f.data, close.data) {
		t.Fatal("normal idle close did not complete a WebSocket handshake")
	}
	watcher.until(t, hasProto(msgPeers, ""))
	ws := join()
	ws.until(t, hasSVG("companion-ring"))
	watcher.until(t, hasProto(msgPeers, c.id))
	if after := s.history(t, c.id, ""); !slices.EqualFunc(rows, after, func(a, b chatEntry) bool { return a.ID == b.ID && a.Text == b.Text }) {
		t.Fatal("idle disconnect/reconnect did not preserve canonical chat")
	}
}

func (s *e2eSuite) gradedEndpointAndQueuedVoice(t *testing.T) {
	if s.real {
		t.Skip("covered with real Silero, Parakeet and Kokoro in --live without extra model prompts")
	}
	t.Run("QuickBox3", func(t *testing.T) {
		c := s.board(t, "esp-box-3", "Quick")
		c.mirrorOn(t, s)
		pcm := s.synth(t, "Why does Jupiter have stripes?")
		c.tap(t, "companion-ring")
		c.op(t, "rec", "action", `"start"`, "endpoint", `"bridge"`)
		_, first := c.measuredPlayback(t, c.capture(t, pcm))
		e2eEndpointWait(t, s.turnStages(t, c, first), tunedEndpoint.quick)
	})
	t.Run("PatientPhoneFollowUp", func(t *testing.T) {
		release := syncOnceClose(s.model.followUp)
		defer release()
		c := s.browser(t, "FollowUp")
		c.timeout = 20 * time.Second
		c.op(t, "capabilities", "bridge_endpoint", "true")
		pcm := s.synth(t, "Once upon a time there was a little frog, and then")
		pcm = append(pcm, make([]byte, sampleRate*2*32/10)...)
		pcm = append(pcm, s.synth(t, "it made a house under a purple mushroom.")...)
		c.text(t, "Virtual test queued follow-up")
		c.scene(t, "rec-continue")
		c.tap(t, "rec-continue")
		c.op(t, "rec", "action", `"start"`, "endpoint", `"bridge"`)
		end := c.capture(t, pcm)
		c.syncFrames(t)
		if rows := s.history(t, c.id, ""); len(rows) != 1 || rows[0].Source != "typed" {
			t.Fatal("follow-up was admitted during the active typed turn")
		}
		release()
		c.op(t, "tts", "state", `"start"`)
		c.expect(t, "original reply PCM", isPlayback)
		firstEnd := c.op(t, "tts", "state", `"end"`)
		if rows := s.history(t, c.id, ""); len(rows) != 2 || rows[1].Role != "companion" {
			t.Fatal("follow-up did not wait for the original reply's playback acknowledgement")
		}
		c.ack(t, firstEnd)
		_, first := c.measuredPlayback(t, end)
		e2eEndpointWait(t, s.turnStages(t, c, first), tunedEndpoint.patient)
		if rows := s.history(t, c.id, ""); len(rows) != 4 || rows[2].Source != "voice" ||
			!strings.Contains(strings.ToLower(rows[2].Text), "purple mushroom") || rows[3].Role != "companion" {
			t.Fatal("queued voice did not preserve both clauses and complete after the first turn")
		}
	})
}

func (s *e2eSuite) menus(t *testing.T, variant, label string) {
	c := s.board(t, variant, label)
	c.mirrorOn(t, s)
	s.browser(t, label+"Peer")
	first := c.tapScene(t, "menu", "menu-intercom")
	for _, id := range []string{"menu-call", "menu-games", "menu-history"} {
		if !strings.Contains(first, id) {
			t.Fatal("missing first page control:", id)
		}
	}
	for _, nav := range []struct{ id, want string }{
		{"menu-intercom", "Talk to…"}, {"menu-call", "Call"}, {"menu-games", "No saved games yet"}, {"menu-history", "No messages yet"},
	} {
		c.tapScene(t, nav.id, nav.want)
		c.tapScene(t, "menu-back", "menu-intercom")
	}
	c.tapScene(t, "menu-next", "menu-memories")
	c.tapScene(t, "menu-memories", "No memories yet")
	c.tapScene(t, "menu-back", "menu-memories")
	c.tapScene(t, "menu-voice", "voice-")
	for i, voice := range voiceCatalog {
		id := "voice-" + strings.ToLower(voice.Name)
		if !strings.Contains(c.svg, id) {
			c.tapScene(t, "menu-next", id)
		}
		c.tap(t, id)
		c.op(t, "chime", "kind", `"tap"`)
		got := testObject(t, s.request(t, "GET", "/api/devices", nil, 200))
		if !slices.ContainsFunc(got["devices"].([]any), func(v any) bool {
			return v.(object)["device_id"] == c.id && v.(object)["voice"] == voice.ID
		}) {
			t.Fatalf("voice choice %d (%s) did not persist", i, voice.Name)
		}
	}
	c.tapScene(t, "menu-back", "menu-volume")
	c.tapScene(t, "menu-volume", "vol-plus")
	c.tap(t, "vol-plus")
	c.op(t, "volume", "persist", "true")
	c.tapScene(t, "menu-back", "menu-volume")
	c.tapScene(t, "menu-back", "companion-ring")
	hasColor := func(r image.Rectangle) bool {
		seen := map[uint16]bool{}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				seen[c.pixels[y*c.width+x]] = true
			}
		}
		return len(seen) >= 3
	}
	if c.codecs[1] == 0 || !hasColor(image.Rect(c.width/4, c.high/4, 3*c.width/4, 3*c.high/4)) {
		t.Fatal("decoded framebuffer has no face artwork or RLE565 packet")
	}
	if variant == "esp-vocat" {
		var st [16]byte
		batt := int16(48)
		binary.LittleEndian.PutUint16(st[2:4], uint16(batt))
		c.sendProto(t, msgTelemetry, st[:])
		f := c.expect(t, "battery dirty rectangle", func(f wsFrame) bool {
			if !isDisplay(f) {
				return false
			}
			r, _, _ := decodeDisplay(t, f)
			return r.Dx() <= 50 && r.Dy() <= 25 && (r.Min.X < 150 || r.Min.X > 210)
		})
		rect, _, _ := decodeDisplay(t, f)
		t.Logf("firmware telemetry produced native dirty rectangle %v", rect)
	}
	t.Logf("%d×%d native: RGB565=%d, RLE565=%d", c.width, c.high, c.codecs[0], c.codecs[1])
}

func (s *e2eSuite) rawFramebuffer(t *testing.T) {
	c := s.board(t, "esp-vocat", "RawPixels")
	c.mirrorOn(t, s)
	// Pick a non-day plateau so the wire test covers the configured timezone
	// and cannot cross an atmosphere bucket transition during the dirty patch.
	var zone string
	var grade gradeKey
	now := time.Now()
	for _, name := range []string{"UTC", "Pacific/Honolulu", "Asia/Tokyo", "Asia/Kolkata", "America/New_York"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		if g := gradeAt(now, loc); g != gradeDay && g < gradeBlend && g == gradeAt(now.Add(2*time.Minute), loc) {
			zone, grade = name, g
			break
		}
	}
	if zone == "" {
		t.Fatal("could not find a stable non-day test timezone")
	}
	// Two otherwise-identical scenes make an incompressible dirty patch.
	const background = `<svg><rect width="360" height="360" fill="black"/><text x="180" y="95">Raw pixels</text>`
	s.tool(t, "app", object{"devices": c.id, "svg": background + "</svg>"}, false)
	c.scene(t, "Raw pixels")
	var mosaic strings.Builder
	mosaic.WriteString(background)
	for y := range 12 {
		for x := range 12 {
			rgb := (uint32(1+x+12*y) * 0x987ab5) & 0xffffff
			fmt.Fprintf(&mosaic, `<rect x="%d" y="%d" width="1" height="1" fill="#%06x"/>`, 170+x, 145+y, rgb)
		}
	}
	mosaic.WriteString("</svg>")
	s.tool(t, "app", object{"devices": c.id, "svg": mosaic.String()}, false)
	f := c.expect(t, "raw RGB565 mosaic rectangle", func(f wsFrame) bool { return isDisplay(f) && f.data[9] == 0 })
	rect, _, pixels := decodeDisplay(t, f)
	lut := &atmosphereLUTs[grade]
	if rect != image.Rect(170, 145, 182, 157) || pixels[0] != pack565(lut[0][0x98], lut[1][0x7a], lut[2][0xb5]) {
		t.Fatal("wrong raw RGB565 dirty patch geometry or byte order:", rect)
	}
	t.Logf("native RGB565 patch %v (%d bytes)", rect, len(f.data))
}

func (s *e2eSuite) intercom(t *testing.T) {
	a, b := s.browser(t, "NoteA"), s.browser(t, "NoteB")
	pcm := s.synth(t, "This is a voice note about a purple kite.")
	start := a.note(t, pcm, b.id)
	b.op(t, "chime", "kind", `"intercom"`)
	// Intercom applies the published voice normalization after the spoken
	// sender-name prefix. It must not transcribe/resynthesize the note.
	spoken := b.playback(t, start)
	if !bytes.HasSuffix(spoken, NormalizeVoicePCM(pcm)) {
		t.Fatal("intercom lost the normalized original recording")
	}
	b.scene(t, "reply")
	b.tap(t, "reply")
	if start := testObject(t, b.op(t, "rec", "action", `"start"`).data); start["target"] != a.id {
		t.Fatal("Reply did not target the original sender")
	}
	reply := s.synth(t, "I received your voice note.")
	now := b.note(t, reply, a.id)
	a.op(t, "chime", "kind", `"intercom"`)
	if !bytes.HasSuffix(a.playback(t, now), NormalizeVoicePCM(reply)) {
		t.Fatal("Reply did not relay the recorded audio back")
	}
}

func e2eRing(t *testing.T, a, b *e2eCompanion) time.Time {
	t.Helper()
	start := time.Now()
	sendCallStartReq(t, a.testWS, b.id)
	a.op(t, "chime", "kind", `"ring"`)
	b.op(t, "chime", "kind", `"ring"`)
	b.scene(t, "call-answer")
	return start
}

func e2eStartCall(t *testing.T, a, b *e2eCompanion) int {
	t.Helper()
	first, second := a.op(t, "call", "state", `"start"`), b.op(t, "call", "state", `"start"`)
	id := int(binary.LittleEndian.Uint32(framePayload(first)[1:5]))
	for _, f := range []wsFrame{first, second} {
		if id == 0 || int(binary.LittleEndian.Uint32(framePayload(f)[1:5])) != id {
			t.Fatal("peers did not negotiate a matching tagged firmware call")
		}
	}
	a.scene(t, "call-mute")
	b.scene(t, "call-mute")
	return id
}

func e2eEndCall(t *testing.T, a, b *e2eCompanion, reason string) {
	t.Helper()
	for _, c := range []*e2eCompanion{a, b} {
		c.op(t, "call", "state", `"end"`, "reason", fmt.Sprintf("%q", reason))
	}
}

func (s *e2eSuite) calls(t *testing.T) {
	for _, variant := range []string{"WebWeb", "WebBoard"} {
		t.Run(variant, func(t *testing.T) {
			a := s.browser(t, "CallFrom")
			var b *e2eCompanion
			if variant == "WebWeb" {
				b = s.browser(t, "CallTo")
			} else {
				b = s.board(t, "esp-box-3", "CallBoard")
				b.mirrorOn(t, s)
			}
			ring := e2eRing(t, a, b)
			b.tap(t, "call-answer")
			e2eStartCall(t, a, b)
			t.Logf("ring → connected %s", time.Since(ring).Round(time.Millisecond))
			pcm := bytes.Repeat([]byte{100, 5}, callPCMBytes/2)
			relay := func(from, to *e2eCompanion, data []byte) {
				t.Helper()
				start := time.Now()
				from.write(t, maskedFrame(wsBinary, true, append([]byte{0x04}, data...)))
				if got := to.expect(t, "relayed call PCM", isPlayback); !bytes.Equal(got.data[1:], data) {
					t.Fatal("call relayed unexpected or mutated PCM")
				}
				t.Logf("tagged microphone → peer PCM %s", time.Since(start).Round(time.Microsecond))
			}
			relay(a, b, pcm)
			relay(b, a, pcm)
			b.tap(t, "call-mute")
			b.op(t, "call", "muted", "true")
			b.scene(t, "Unmute")
			b.write(t, maskedFrame(wsBinary, true, append([]byte{0x04}, pcm...)))
			b.syncFrames(t)
			relay(a, b, pcm) // Mute does not silence downlink.
			b.tap(t, "call-mute")
			b.op(t, "call", "muted", "false")
			b.scene(t, `aria-label=\"Mute\"`)
			relay(b, a, bytes.Repeat([]byte{5, 100}, callPCMBytes/2))
			b.tap(t, "call-hangup")
			e2eEndCall(t, a, b, "hung up")
			a.expect(t, "call history event", func(f wsFrame) bool {
				return f.op == wsText && bytes.Contains(f.data, []byte(`"type":"chat"`)) &&
					bytes.Contains(f.data, []byte(`"source":"call"`))
			})
			if !slices.ContainsFunc(s.history(t, a.id, ""), func(row chatEntry) bool {
				return row.Source == "call" && strings.Contains(row.Text, "Voice call with")
			}) {
				t.Fatal("completed call did not enter canonical history")
			}
		})
	}
	t.Run("BusyDeclineGlare", func(t *testing.T) {
		a, b, third := s.browser(t, "GlareA"), s.browser(t, "GlareB"), s.browser(t, "Busy")
		e2eRing(t, a, b)
		sendCallStartReq(t, third.testWS, b.id)
		third.scene(t, "busy.")
		b.tap(t, "call-decline")
		e2eEndCall(t, a, b, "declined")
		e2eRing(t, a, b)
		sendCallStartReq(t, b.testWS, a.id)
		e2eStartCall(t, a, b) // The reciprocal request answers, not starts a second call.
		a.tap(t, "call-hangup")
		e2eEndCall(t, a, b, "hung up")
	})
}

func (s *e2eSuite) callTimeout(t *testing.T) {
	a, b := s.browser(t, "TimeoutA"), s.browser(t, "TimeoutB")
	a.timeout, b.timeout = 32*time.Second, 32*time.Second
	start := e2eRing(t, a, b)
	e2eEndCall(t, a, b, "no answer")
	if elapsed := time.Since(start); elapsed < 24*time.Second || elapsed > 29*time.Second {
		t.Fatalf("unexpected no-answer timing: %s", elapsed)
	} else {
		t.Logf("ring → no answer %s", elapsed.Round(time.Millisecond))
	}
}

func (s *e2eSuite) idleWake(t *testing.T, c *e2eCompanion, start time.Time) {
	c.timeout = 130 * time.Second
	c.op(t, "power", "brightness", "12")
	if elapsed := time.Since(start); elapsed < 118*time.Second || elapsed > 125*time.Second {
		t.Fatalf("unexpected idle sleep timing: %s", elapsed)
	} else {
		t.Logf("idle → panel-off power %s", elapsed.Round(time.Millisecond))
	}
	c.syncFrames(t)
	start = time.Now()
	c.input(t, "tap", 180, 145, c.revision)
	c.op(t, "power", "brightness", "85")
	c.write(t, maskedFrame(wsPing, true, []byte("wake")))
	c.expect(t, "wake without also starting Talk", func(f wsFrame) bool {
		if e2eFields(f, "rec", "action", `"start"`) {
			t.Fatal("wake tap also started recording")
		}
		return f.op == wsPong && string(f.data) == "wake"
	})
	t.Logf("wake touch → active power %s", time.Since(start).Round(time.Millisecond))
}

func (s *e2eSuite) scopedTunnel(t *testing.T) {
	c := s.browser(t, "Share")
	c.testWS.conn.Close()
	other := s.browser(t, "NotShared")
	scope := s.scope(t, c.id)
	get := func(path string, code int) []byte { return e2eRequest(t, scope, "GET", path, "", nil, code) }
	get("/app", 200)
	worker := get("/app/sw.js", 200)
	if !bytes.Contains(worker, []byte("lark-shell-scoped-")) ||
		!bytes.Contains(worker, []byte(`"/app/lark.svg"`)) {
		t.Fatal("scoped app did not serve the restricted offline shell")
	}
	manifest := testObject(t, get(pwaPath+"manifest.webmanifest?token="+url.QueryEscape(scope.token), 200))
	icons, _ := manifest["icons"].([]any)
	if manifest["display"] != "standalone" || manifest["orientation"] != "portrait" || len(icons) != 3 {
		t.Fatal("scoped Phone Lark did not serve an installable portrait manifest")
	}
	for _, icon := range icons {
		raw := get(str(icon.(object)["src"]), 200)
		if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatal("scoped manifest icon is not a PNG")
		}
	}
	info := testObject(t, get("/api/scope", 200))
	if info["device_id"] != c.id || info["scoped"] != true {
		t.Fatal("scoped listener resolved the wrong virtual cat")
	}
	for _, path := range []string{"/api/devices", "/api/parent", "/api/diagnostics", "/mcp"} {
		get(path, 404)
	}
	get("/api/devices/"+c.id+"/messages", 200)
	get("/api/devices/"+other.id+"/messages", 403)
	ws := testConnectWS(t, scope.base, scope.token, true)
	ws.helloWeb(t, "web-scope001", c.id, capCall|capDrag|capBridgeEndpoint, false)
	client := &e2eCompanion{testWS: ws, id: c.id, name: c.name}
	client.timeout = 20 * time.Second
	ws.sendProto(t, msgWatch, appendStr8(nil, other.id))
}

func (s *e2eSuite) diagnostics(t *testing.T) {
	if app := s.request(t, "GET", "/app", nil, 200); !bytes.Contains(app, []byte(`id="stage"`)) {
		t.Fatal("browser entry point is not the cat app")
	}
	for _, asset := range []string{"client.js", "style.css", "mic.js"} {
		s.request(t, "GET", "/app/"+asset, nil, 200)
	}
	unauthorized := s.liveClient
	unauthorized.token = ""
	e2eRequest(t, unauthorized, "GET", "/api/diagnostics", "", nil, 401)
	diag := testObject(t, s.request(t, "GET", "/api/diagnostics", nil, 200))
	for _, key := range []string{"bridge", "devices", "services", "recent_turns"} {
		if _, ok := diag[key]; !ok {
			t.Errorf("missing diagnostic section %s", key)
		}
	}
	probe := liveJSON[object](t, s.liveClient, "/api/diagnostics/stt", nil, []byte{}, "", nil)
	if !strings.EqualFold(str(probe["transcript"]), "The moon reflects sunlight.") || e2eNumber(probe["clip_ms"]) < 500 {
		t.Fatal("isolated STT diagnostic could not transcribe the reference clip")
	}
	t.Logf("diagnostic Parakeet: %v ms on %v ms PCM", probe["stt_ms"], probe["clip_ms"])
}

func (s *e2eSuite) historyPage(t *testing.T, id string, cursor *historyCursor) historyPage {
	t.Helper()
	q := url.Values{"limit": {"1"}}
	if cursor != nil {
		q.Set("before_ts", fmt.Sprint(cursor.TS))
		q.Set("before_id", cursor.ID)
	}
	var page historyPage
	if err := json.Unmarshal(s.request(t, "GET", "/api/devices/"+id+"/messages?"+q.Encode(), nil, 200), &page); err != nil {
		t.Fatal("could not decode history page")
	}
	return page
}
