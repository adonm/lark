package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pierrec/lz4/v4"
)

type simBoardSpec struct {
	name   string
	width  int
	height int
	shape  uint8
	caps   uint32
}

var simBoards = map[string]simBoardSpec{
	"vocat": {
		name:   "esp-vocat",
		width:  360,
		height: 360,
		shape:  shapeRound,
		caps:   capBattery | capDrag | capPointerRev | capBridgeEndpoint | capPanelSleep,
	},
	"box3": {
		name:   "esp-box-3",
		width:  320,
		height: 240,
		shape:  shapeRect,
		caps:   capDrag | capPointerRev | capBridgeEndpoint,
	},
}

func defaultSimID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "sim-local"
	}
	var b strings.Builder
	b.WriteString("sim-")
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

type simTerminal struct {
	id      string
	spec    simBoardSpec
	tcp     net.Conn
	udp     *net.UDPConn
	srvUDP  *net.UDPAddr
	ssrc    uint32
	client  *cliClient
	started time.Time
	seq     uint16
	rtpSeq  uint16
	rtpTS   uint32
	recSess uint16

	mu            sync.Mutex
	cond          *sync.Cond
	fb            []uint16
	frameRev      uint16
	frameCount    int
	frameBytes    int
	lastFrameAt   time.Time
	state         string
	recActive     bool
	recBridgeEP   bool
	recListen     bool
	recCtrlAt     time.Time
	lastTapAt     time.Time
	speechEndAt   time.Time
	ttsAudioID    uint64
	ttsActive     bool
	ttsStartAt    time.Time
	ttsFirstPCMAt time.Time
	ttsBytes      int
	ttsBuf        []byte
	timeline      []object
	closed        bool
}

func newSimTerminal(id string, spec simBoardSpec, tcp net.Conn, udp *net.UDPConn, srvUDP *net.UDPAddr, ssrc uint32, client *cliClient) *simTerminal {
	s := &simTerminal{
		id:      id,
		spec:    spec,
		tcp:     tcp,
		udp:     udp,
		srvUDP:  srvUDP,
		ssrc:    ssrc,
		client:  client,
		started: time.Now(),
		fb:      make([]uint16, spec.width*spec.height),
		state:   "idle",
		recSess: 1,
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *simTerminal) sendProto(msgType uint8, payload []byte) error {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	pkt := packProtoPacket(msgType, seq, payload)
	_, err := s.tcp.Write(pkt)
	return err
}

func (s *simTerminal) readLoop() {
	defer func() {
		s.mu.Lock()
		s.closed = true
		s.cond.Broadcast()
		s.mu.Unlock()
	}()
	for {
		mt, _, payload, err := readProtoFrame(s.tcp)
		if err != nil {
			return
		}
		now := time.Now()
		switch mt {
		case msgPing:
			_ = s.sendProto(msgPong, payload)
		case msgFrame:
			s.mu.Lock()
			if rev, ok := decodeFrameInto(s.fb, s.spec.width, s.spec.height, payload); ok {
				s.frameRev = rev
				s.frameCount++
				s.frameBytes += len(payload)
				s.lastFrameAt = now
				s.cond.Broadcast()
			}
			s.mu.Unlock()
		case msgStatus:
			if msg, ok := decodeProtoStatus(payload); ok {
				s.mu.Lock()
				s.state = statusName(msg.state)
				s.timeline = append(s.timeline, object{"event": "status", "state": s.state, "t_ms": now.Sub(s.started).Milliseconds()})
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		case msgRecCtrl:
			if msg, ok := decodeProtoRecCtrl(payload); ok {
				s.mu.Lock()
				switch msg.op {
				case recOpStart:
					s.recActive = true
					s.recSess = msg.session
					s.recBridgeEP = msg.flags&recTargetBridgeE != 0
					s.recListen = msg.flags&recTargetListen != 0
					s.recCtrlAt = now
					s.timeline = append(s.timeline, object{"event": "rec_start", "t_ms": now.Sub(s.started).Milliseconds()})
					ack := protoRecStart{session: msg.session}.encode()
					go func() { _ = s.sendProto(msgRecStart, ack) }()
				case recOpStop, recOpCancel:
					s.recActive = false
					s.timeline = append(s.timeline, object{"event": "rec_stop", "t_ms": now.Sub(s.started).Milliseconds()})
				}
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		case msgTTSCtrl:
			if msg, ok := decodeProtoTTSCtrl(payload); ok {
				s.mu.Lock()
				switch msg.op {
				case ttsOpStart:
					s.ttsAudioID = msg.audioID
					s.ttsActive = true
					s.ttsStartAt = now
					s.ttsFirstPCMAt = time.Time{}
					s.ttsBuf = s.ttsBuf[:0]
					s.timeline = append(s.timeline, object{"event": "tts_start", "t_ms": now.Sub(s.started).Milliseconds()})
				case ttsOpEnd, ttsOpStop:
					s.ttsActive = false
					s.timeline = append(s.timeline, object{"event": "tts_end", "t_ms": now.Sub(s.started).Milliseconds()})
					if msg.op == ttsOpEnd {
						// Firmware acknowledges after playback, not when the
						// end frame is queued. Pacing the ack keeps the
						// speaking state observable, so Stop can land there.
						go func(id uint64, d time.Duration) {
							time.Sleep(min(d, 3*time.Second))
							_ = s.sendProto(msgTTSAck, encodeTTSAck(false, id))
						}(msg.audioID, pcmDuration(len(s.ttsBuf)))
					}
				}
				s.cond.Broadcast()
				s.mu.Unlock()
			}
		case msgTTSPCM:
			s.mu.Lock()
			if s.ttsFirstPCMAt.IsZero() {
				s.ttsFirstPCMAt = now
				s.timeline = append(s.timeline, object{"event": "tts_first_pcm", "t_ms": now.Sub(s.started).Milliseconds()})
			}
			s.ttsBytes += len(payload)
			s.ttsBuf = append(s.ttsBuf, payload...)
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	}
}

func statusName(st uint8) string {
	switch st {
	case statusIdle:
		return "idle"
	case statusListening:
		return "listening"
	case statusThinking:
		return "thinking"
	case statusSpeaking:
		return "speaking"
	case statusBlocked:
		return "blocked"
	case statusDone:
		return "done"
	case statusError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", st)
	}
}

func decodeFrameInto(fb []uint16, w, h int, pkt []byte) (uint16, bool) {
	if len(pkt) < 12 || pkt[0] != frameTag {
		return 0, false
	}
	rx := int(binary.LittleEndian.Uint16(pkt[1:3]))
	ry := int(binary.LittleEndian.Uint16(pkt[3:5]))
	rw := int(binary.LittleEndian.Uint16(pkt[5:7]))
	rh := int(binary.LittleEndian.Uint16(pkt[7:9]))
	codec := pkt[9]
	rev := binary.LittleEndian.Uint16(pkt[10:12])
	payload := pkt[12:]
	if rw == 0 || rh == 0 || rx+rw > w || ry+rh > h {
		return 0, false
	}
	pixels := rw * rh
	var rawBE []byte
	switch codec {
	case frameCodecRaw:
		if len(payload) < pixels*2 {
			return 0, false
		}
		rawBE = payload[:pixels*2]
	case frameCodecRLE565:
		rawBE = make([]byte, 0, pixels*2)
		for i := 0; i < len(payload) && len(rawBE) < pixels*2; {
			tag := payload[i]
			i++
			run := int(tag&0x7f) + 1
			if tag&0x80 != 0 {
				if i+2 > len(payload) {
					return 0, false
				}
				hi, lo := payload[i], payload[i+1]
				i += 2
				for range run {
					rawBE = append(rawBE, hi, lo)
				}
			} else {
				need := run * 2
				if i+need > len(payload) {
					return 0, false
				}
				rawBE = append(rawBE, payload[i:i+need]...)
				i += need
			}
		}
		if len(rawBE) != pixels*2 {
			return 0, false
		}
	case frameCodecLZ4:
		rawBE = make([]byte, pixels*2)
		n, err := lz4.UncompressBlock(payload, rawBE)
		if err != nil || n != pixels*2 {
			return 0, false
		}
	case frameCodecJPEG:
		img, err := jpeg.Decode(bytes.NewReader(payload))
		if err != nil {
			return 0, false
		}
		b := img.Bounds()
		if b.Dx() != rw || b.Dy() != rh {
			return 0, false
		}
		for y := range rh {
			for x := range rw {
				r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				fb[(ry+y)*w+(rx+x)] = pack565(uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			}
		}
		return rev, true
	default:
		return 0, false
	}
	off := 0
	for y := range rh {
		for x := range rw {
			fb[(ry+y)*w+(rx+x)] = uint16(rawBE[off])<<8 | uint16(rawBE[off+1])
			off += 2
		}
	}
	return rev, true
}

func writeFBToPNG(fb []uint16, w, h int, path string) error {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := fb[y*w+x]
			r5 := uint8(v >> 11)
			g6 := uint8((v >> 5) & 0x3f)
			b5 := uint8(v & 0x1f)
			img.SetRGBA(x, y, imagecolor.RGBA{
				R: (r5 << 3) | (r5 >> 2),
				G: (g6 << 2) | (g6 >> 4),
				B: (b5 << 3) | (b5 >> 2),
				A: 255,
			})
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func readWAVPCM16k(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseWAVPCM16k(raw)
}

func parseWAVPCM16k(raw []byte) ([]byte, error) {
	if len(raw) < 44 || !bytes.Equal(raw[0:4], []byte("RIFF")) || !bytes.Equal(raw[8:12], []byte("WAVE")) {
		return nil, errors.New("expected RIFF/WAVE file")
	}
	off := 12
	var rate uint32
	var channels, bits uint16
	for off+8 <= len(raw) {
		id := string(raw[off : off+4])
		sz := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		off += 8
		if off+sz > len(raw) {
			return nil, errors.New("truncated WAV chunk")
		}
		switch id {
		case "fmt ":
			if sz < 16 {
				return nil, errors.New("short WAV fmt chunk")
			}
			channels = binary.LittleEndian.Uint16(raw[off+2 : off+4])
			rate = binary.LittleEndian.Uint32(raw[off+4 : off+8])
			bits = binary.LittleEndian.Uint16(raw[off+14 : off+16])
			if channels != 1 || rate != sampleRate || bits != 16 {
				return nil, fmt.Errorf("expected 16kHz 16-bit mono WAV, got %dHz %d-bit %dch", rate, bits, channels)
			}
		case "data":
			if rate == 0 {
				return nil, errors.New("WAV missing fmt chunk before data")
			}
			return raw[off : off+sz], nil
		}
		off += sz + (sz & 1)
	}
	return nil, errors.New("WAV missing data chunk")
}

func writeWAV16k(path string, pcm []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	hdr := wavHeader(len(pcm))
	out := make([]byte, len(hdr)+len(pcm))
	copy(out, hdr[:])
	copy(out[len(hdr):], pcm)
	return os.WriteFile(path, out, 0o644)
}

func (s *simTerminal) ttsWAV(ctx context.Context, text string) ([]byte, error) {
	body, _ := json.Marshal(object{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.base+"/api/tts", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Lark-Token", s.client.token)
	req.Header.Set("Content-Type", jsonType)
	res, err := s.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("TTS HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseWAVPCM16k(raw)
}

func (s *simTerminal) sendTap(x, y int) error {
	s.mu.Lock()
	rev := s.frameRev
	s.lastTapAt = time.Now()
	s.mu.Unlock()
	return s.sendProto(msgPointer, encodePointer(ptrTap, uint16(x), uint16(y), rev))
}

func (s *simTerminal) waitFor(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer timer.Stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	for !pred() && !s.closed && time.Now().Before(deadline) {
		s.cond.Wait()
	}
	return pred()
}

func (s *simTerminal) stateReached(want string) bool {
	switch want {
	case "idle":
		return s.state == "idle" || s.state == "done"
	case "thinking":
		// A slow model call keeps the status here; a fast one may already be
		// speaking or done. Accept a thinking status seen earlier in the turn
		// so the wait cannot miss the transition.
		return s.state == "thinking" || s.state == "speaking" || s.state == "done" || s.sawState("thinking")
	case "speaking":
		return s.state == "speaking" || (s.state == "done" && s.ttsBytes > 0) || s.sawState("speaking")
	default:
		return s.state == want
	}
}

func (s *simTerminal) sawState(want string) bool {
	for _, event := range s.timeline {
		if len(event) != 0 && event["event"] == "status" && event["state"] == want {
			return true
		}
	}
	return false
}

func (s *simTerminal) execLine(ctx context.Context, line string) (object, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
		return nil, nil
	}
	t0 := time.Now()
	cmd := fields[0]
	out := object{"cmd": cmd}
	switch cmd {
	case "attach":
		if len(fields) != 2 {
			return nil, errors.New("usage: attach <companion>")
		}
		s.mu.Lock()
		prevFrames := s.frameCount
		s.mu.Unlock()
		if _, err := s.client.do(ctx, http.MethodPost, "/api/terminals/"+s.id+"/attach", object{"companion_id": fields[1]}); err != nil {
			return nil, err
		}
		s.waitFor(5*time.Second, func() bool { return s.frameCount > prevFrames })
		out["companion"] = fields[1]
	case "tap":
		if len(fields) != 3 {
			return nil, errors.New("usage: tap <x> <y>")
		}
		x, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		y, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		prevFrames := s.frameCount
		s.mu.Unlock()
		if err := s.sendTap(x, y); err != nil {
			return nil, err
		}
		s.waitFor(500*time.Millisecond, func() bool { return s.frameCount > prevFrames || s.recActive })
		out["x"], out["y"] = x, y
	case "tap-label":
		if len(fields) < 2 {
			return nil, errors.New("usage: tap-label <label>")
		}
		label := strings.Join(fields[1:], " ")
		var res object
		var x, y int
		var matched string
		var err error
		deadline := time.Now().Add(500 * time.Millisecond)
		for {
			res, err = s.client.do(ctx, http.MethodGet, "/api/terminals/"+s.id+"/hits", nil)
			if err != nil {
				return nil, err
			}
			x, y, matched, err = resolveHitLabel(res, label)
			if err == nil || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			return nil, err
		}
		if rev, ok := numInt(res["rev"]); ok && rev > 0 {
			s.waitFor(500*time.Millisecond, func() bool { return s.frameRev == uint16(rev) })
		}
		s.mu.Lock()
		prevFrames := s.frameCount
		s.mu.Unlock()
		if err := s.sendTap(x, y); err != nil {
			return nil, err
		}
		s.waitFor(500*time.Millisecond, func() bool { return s.frameCount > prevFrames || s.recActive })
		out["label"], out["matched"], out["x"], out["y"] = label, matched, x, y
	case "say":
		if len(fields) < 2 {
			return nil, errors.New("usage: say <wav|text>")
		}
		arg := strings.TrimSpace(strings.TrimPrefix(line, "say"))
		var pcm []byte
		var err error
		if strings.HasSuffix(strings.ToLower(arg), ".wav") {
			pcm, err = readWAVPCM16k(arg)
		} else if _, statErr := os.Stat(arg); statErr == nil {
			pcm, err = readWAVPCM16k(arg)
		} else {
			pcm, err = s.ttsWAV(ctx, arg)
		}
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		active := s.recActive
		s.mu.Unlock()
		if !active {
			s.recSess++
			b := binary.LittleEndian.AppendUint16(nil, s.recSess)
			b = appendStr8(b, "")
			if err := s.sendProto(msgRecStart, b); err != nil {
				return nil, err
			}
			if !s.waitFor(2*time.Second, func() bool { return s.recActive }) {
				return nil, errors.New("timed out waiting for REC_CTRL start")
			}
		}
		pkt := make([]byte, rtpPacketBytes)
		sentBytes := 0
		for off := 0; off < len(pcm); off += rtpPCMBytes {
			s.mu.Lock()
			stillActive := s.recActive
			s.mu.Unlock()
			if !stillActive {
				break
			}
			var chunk [rtpPCMBytes]byte
			copy(chunk[:], pcm[off:min(len(pcm), off+rtpPCMBytes)])
			s.rtpSeq++
			s.rtpTS += rtpSamples
			n := encodeRTPPacket(pkt, rtpPTRecPCM, s.rtpSeq, s.rtpTS, s.ssrc, chunk[:])
			if _, err := s.udp.WriteToUDP(pkt[:n], s.srvUDP); err != nil {
				return nil, err
			}
			sentBytes += rtpPCMBytes
			time.Sleep(20 * time.Millisecond)
		}
		s.mu.Lock()
		s.speechEndAt = time.Now()
		stillActive := s.recActive
		bridgeEP := s.recBridgeEP
		s.mu.Unlock()
		if stillActive && bridgeEP {
			// Feed silence tail so Silero endpoint detects end-of-speech. The
			// bridge's cap is 1.2 s, so leave a little margin.
			var silence [rtpPCMBytes]byte
			for range 90 {
				s.mu.Lock()
				ok := s.recActive
				s.mu.Unlock()
				if !ok {
					break
				}
				s.rtpSeq++
				s.rtpTS += rtpSamples
				n := encodeRTPPacket(pkt, rtpPTRecPCM, s.rtpSeq, s.rtpTS, s.ssrc, silence[:])
				_, _ = s.udp.WriteToUDP(pkt[:n], s.srvUDP)
				time.Sleep(20 * time.Millisecond)
			}
		}
		s.mu.Lock()
		stillActive = s.recActive
		s.mu.Unlock()
		if stillActive {
			b := binary.LittleEndian.AppendUint16(nil, s.recSess)
			b = append(b, 0)
			b = appendStr8(b, "")
			_ = s.sendProto(msgRecEnd, b)
		}
		out["bytes"] = sentBytes
	case "wait":
		if len(fields) < 2 {
			return nil, errors.New("usage: wait <state> [timeout]")
		}
		want := fields[1]
		timeout := 30 * time.Second
		if len(fields) >= 3 {
			if d, err := time.ParseDuration(fields[2]); err == nil {
				timeout = d
			}
		}
		ok := s.waitFor(timeout, func() bool { return s.stateReached(want) })
		if !ok {
			s.mu.Lock()
			got := s.state
			s.mu.Unlock()
			return nil, fmt.Errorf("timed out waiting for state %q (current %q)", want, got)
		}
		out["state"] = want
	case "wait-current":
		if len(fields) < 2 {
			return nil, errors.New("usage: wait-current <state> [timeout]")
		}
		want := fields[1]
		timeout := 30 * time.Second
		if len(fields) >= 3 {
			if d, err := time.ParseDuration(fields[2]); err == nil {
				timeout = d
			}
		}
		ok := s.waitFor(timeout, func() bool { return s.state == want })
		if !ok {
			s.mu.Lock()
			got := s.state
			s.mu.Unlock()
			return nil, fmt.Errorf("timed out waiting for current state %q (current %q)", want, got)
		}
		out["state"] = want
	case "sleep":
		if len(fields) != 2 {
			return nil, errors.New("usage: sleep <ms>")
		}
		ms, err := strconv.Atoi(fields[1])
		if err != nil || ms < 0 || ms > 30000 {
			return nil, errors.New("sleep must be 0..30000 ms")
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		out["ms"] = ms
	case "state":
		s.mu.Lock()
		out["state"] = s.state
		s.mu.Unlock()
	case "wait-rec":
		timeout := 10 * time.Second
		if len(fields) >= 2 {
			if d, err := time.ParseDuration(fields[1]); err == nil {
				timeout = d
			}
		}
		if !s.waitFor(timeout, func() bool { return s.recActive }) {
			return nil, fmt.Errorf("timed out waiting for REC_CTRL start")
		}
		out["rec"] = true
	case "hits":
		res, err := s.client.do(ctx, http.MethodGet, "/api/terminals/"+s.id+"/hits", nil)
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"screen", "rev"} {
			if v, ok := res[key]; ok {
				out[key] = v
			}
		}
		if hits, ok := res["hits"].([]any); ok {
			out["hits"] = hits
		}
	case "screen":
		if len(fields) != 2 {
			return nil, errors.New("usage: screen <out.png>")
		}
		s.mu.Lock()
		fbCopy := append([]uint16(nil), s.fb...)
		rev := s.frameRev
		s.mu.Unlock()
		if err := writeFBToPNG(fbCopy, s.spec.width, s.spec.height, fields[1]); err != nil {
			return nil, err
		}
		out["path"], out["rev"] = fields[1], rev
	case "record":
		if len(fields) != 2 {
			return nil, errors.New("usage: record <out.wav>")
		}
		s.waitFor(30*time.Second, func() bool { return !s.ttsActive && len(s.ttsBuf) > 0 })
		s.mu.Lock()
		pcm := append([]byte(nil), s.ttsBuf...)
		s.mu.Unlock()
		if err := writeWAV16k(fields[1], pcm); err != nil {
			return nil, err
		}
		out["path"], out["bytes"] = fields[1], len(pcm)
	case "timeline":
		s.mu.Lock()
		out["events"] = append([]object(nil), s.timeline...)
		s.mu.Unlock()
	default:
		return nil, fmt.Errorf("unknown sim command %q", cmd)
	}

	s.mu.Lock()
	elapsed := time.Since(s.started).Seconds()
	fps := 0.0
	if elapsed > 0 {
		fps = float64(s.frameCount) / elapsed
	}
	out["ms"] = time.Since(t0).Milliseconds()
	out["fps"] = float64(int(fps*10)) / 10
	if _, hasBytes := out["bytes"]; !hasBytes {
		out["bytes"] = s.frameBytes
	}
	if !s.lastTapAt.IsZero() && !s.recCtrlAt.IsZero() && s.recCtrlAt.After(s.lastTapAt) {
		out["tap_to_rec_ctrl_ms"] = s.recCtrlAt.Sub(s.lastTapAt).Milliseconds()
	}
	if !s.speechEndAt.IsZero() && !s.ttsFirstPCMAt.IsZero() && s.ttsFirstPCMAt.After(s.speechEndAt) {
		out["speech_end_to_first_tts_ms"] = s.ttsFirstPCMAt.Sub(s.speechEndAt).Milliseconds()
	}
	s.mu.Unlock()
	return out, nil
}

func resolveHitLabel(res object, label string) (int, int, string, error) {
	hits, _ := res["hits"].([]any)
	want := strings.ToLower(strings.TrimSpace(label))
	for _, item := range hits {
		m, _ := item.(object)
		lbl := str(m["label"])
		id := str(m["id"])
		if strings.ToLower(lbl) == want || strings.ToLower(id) == want {
			x1, _ := numInt(m["x1"])
			y1, _ := numInt(m["y1"])
			x2, _ := numInt(m["x2"])
			y2, _ := numInt(m["y2"])
			matched := lbl
			if matched == "" {
				matched = id
			}
			return (x1 + x2) / 2, (y1 + y2) / 2, matched, nil
		}
	}
	return 0, 0, "", fmt.Errorf("hit label %q not found on screen %q", label, str(res["screen"]))
}

func numInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func runSim(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	board := fs.String("board", "vocat", "board geometry (vocat or box3)")
	id := fs.String("id", defaultSimID(), "terminal ID")
	addr := fs.String("addr", "127.0.0.1:"+defaultDevPort, "server device TCP address")
	scriptPath := fs.String("script", "", "script file (default stdin)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: lark sim [--board vocat|box3] [--id sim-<x>] [--addr host:port] [--script file]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	spec, ok := simBoards[*board]
	if !ok {
		return fmt.Errorf("unknown board %q (expected vocat or box3)", *board)
	}
	client, err := newCLIClient()
	if err != nil {
		return err
	}
	token, err := loadOTAToken()
	if err != nil {
		return err
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return err
	}
	defer udp.Close()
	localUDPPort := uint16(udp.LocalAddr().(*net.UDPAddr).Port)

	tcp, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", *addr)
	if err != nil {
		return err
	}
	defer tcp.Close()

	hello := terminalHello{
		protoVersion:   protoVersion1,
		kind:           termKindHardware,
		capsMask:       spec.caps,
		displayFormats: dispRGB565,
		frameCodecs:    codecBitAll,
		width:          uint16(spec.width),
		height:         uint16(spec.height),
		shape:          spec.shape,
		udpPort:        localUDPPort,
		terminalID:     *id,
		board:          spec.name,
		firmware:       "sim-0.19.0",
		token:          token,
	}
	if _, err := tcp.Write(packProtoPacket(msgHello, 1, encodeTerminalHello(hello))); err != nil {
		return err
	}
	mt, _, payload, err := readProtoFrame(tcp)
	if err != nil {
		return err
	}
	if mt == msgReject {
		return fmt.Errorf("server rejected hello: %s", string(payload))
	}
	if mt != msgWelcome || len(payload) < 20 {
		return fmt.Errorf("expected WELCOME, got %#x", mt)
	}
	srvUDPPort := int(binary.LittleEndian.Uint16(payload[2:4]))
	ssrc := binary.LittleEndian.Uint32(payload[4:8])
	tcpHost, _, _ := net.SplitHostPort(*addr)
	srvUDP, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(tcpHost, strconv.Itoa(srvUDPPort)))
	if err != nil {
		return err
	}

	sim := newSimTerminal(*id, spec, tcp, udp, srvUDP, ssrc, client)
	go sim.readLoop()

	var in io.Reader = stdin
	if *scriptPath != "" {
		f, err := os.Open(*scriptPath)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := sim.execLine(ctx, sc.Text())
		if err != nil {
			b, _ := json.Marshal(object{"cmd": strings.Fields(sc.Text())[0], "ok": false, "error": err.Error()})
			fmt.Fprintln(stdout, string(b))
			return err
		}
		if res != nil {
			res["ok"] = true
			b, _ := json.Marshal(res)
			fmt.Fprintln(stdout, string(b))
		}
	}
	return sc.Err()
}
