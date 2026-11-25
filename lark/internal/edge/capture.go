package edge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"lark/internal/audio"
	proto "lark/internal/proto"
)

type cancelReason uint8

const (
	cancelSend cancelReason = iota
	cancelUser
	cancelWindow
)

type micPacket struct {
	pcm []byte
	at  time.Time
}

type probeResult struct {
	at   time.Duration
	n    int
	text string
	err  error
}

// captureState is one open microphone window. The RTP hot path writes packets
// through cap.packets; only this goroutine reads them.
type captureState struct {
	t      *Terminal
	listen bool
	limit  time.Duration
	grace  time.Duration
	name   string

	packets  chan micPacket
	free     chan []byte
	ctx      context.Context
	cancelFn context.CancelFunc
	cancelCh chan cancelReason
	done     chan struct{}

	// Device capture mode (capture.start), zero otherwise.
	device    bool
	captureID string
	outPath   string
	seconds   int

	session uint16
	reason  atomic.Int32
}

func newCapture(t *Terminal, listen bool, limit time.Duration, name string) *captureState {
	ctx, cancel := context.WithCancel(context.Background())
	c := &captureState{
		t: t, listen: listen, limit: limit, name: name,
		packets: make(chan micPacket, 256), free: make(chan []byte, 64),
		ctx: ctx, cancelFn: cancel, cancelCh: make(chan cancelReason, 1),
		done: make(chan struct{}),
	}
	for range cap(c.free) {
		c.free <- make([]byte, 640)
	}
	return c
}

func (c *captureState) cancel(reason cancelReason) {
	c.reason.Store(int32(reason))
	select {
	case c.cancelCh <- reason:
	default:
	}
}

func (c *captureState) putSlot(b []byte) {
	select {
	case c.free <- b:
	default:
	}
}

func (c *captureState) getSlot() []byte {
	select {
	case b := <-c.free:
		return b
	default:
		return nil
	}
}

func (t *Terminal) beginCapture(listen bool, windowS int, followUp bool, name string) {
	if t.capture != nil {
		t.capture.cancel(cancelUser)
	}
	limit := 20 * time.Second
	if windowS > 0 {
		limit = time.Duration(windowS) * time.Second
	}
	if limit > endpointLimit {
		limit = endpointLimit
	}
	c := newCapture(t, listen, limit, name)
	if followUp {
		c.grace = 400 * time.Millisecond
	}
	c.session = uint16(t.nextTurn())
	var flags uint8
	if t.caps&proto.CapBridgeEndpoint != 0 {
		flags |= proto.RecTargetBridgeE
	}
	if listen {
		flags |= proto.RecTargetListen
	}
	t.ctrl(proto.MsgRecCtrl, proto.ProtoRecCtrl{Op: proto.RecOpStart, Flags: flags, Session: c.session}.Encode())
	t.capture = c
	t.cap.Store(c)
	t.mode = modeListening
	if !listen {
		t.mode = modeRecording
	}
	go c.run()
}

func (t *Terminal) ingestRTP(pcm []byte, at time.Time) {
	c := t.cap.Load()
	if c == nil {
		return
	}
	slot := c.getSlot()
	if slot == nil {
		return
	}
	n := copy(slot, pcm)
	pkt := micPacket{pcm: slot[:n], at: at}
	select {
	case c.packets <- pkt:
	default:
		c.putSlot(slot)
	}
}

func (c *captureState) run() {
	defer close(c.done)
	defer c.finish()
	if c.device {
		c.runDevice()
		return
	}
	vad := c.t.e.models.newVAD()
	if vad == nil {
		vad = &energyVAD{}
	}
	u := &utterance{pcm: make([]byte, 0, utteranceBytes(c.limit))}
	var (
		probeCh   <-chan probeResult
		probeAt   time.Duration
		probeN    int
		probeText string
		specSeq   uint64
		specAt    time.Duration
		specLive  bool
		smartAt   time.Duration = -1
		smartProb float32       = -1
		speechAt  time.Time
		lastTick  time.Time
	)
	probeCtx, cancelProbes := context.WithCancel(c.ctx)
	defer cancelProbes()

	launchProbe := func(n int, at time.Duration) {
		clip := append([]byte(nil), u.pcm[:n]...)
		ch := make(chan probeResult, 1)
		probeCh = ch
		ctx := probeCtx
		go func() {
			text, err := c.t.transcribe(ctx, clip, true)
			ch <- probeResult{at: at, n: n, text: text, err: err}
		}()
	}

	classify := func(at time.Duration) decision {
		if u.policy.voiced && !u.policy.inVoice {
			silence := at - u.policy.lastVoicedAt
			if silence >= endpointSpecQuiet && smartAt != u.policy.lastVoicedAt {
				smartAt = u.policy.lastVoicedAt
				smartProb = c.t.predict(u.pcm)
			}
		}
		return u.policy.classify(at, endpointCap+c.grace, c.limit, smartProb, c.listen)
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case reason := <-c.cancelCh:
			switch reason {
			case cancelUser:
				c.emitCancel(specSeq, "user")
				return
			case cancelWindow:
				c.emitCancel(specSeq, "window")
				return
			default:
				c.commit(u, probeText, probeN, speechAt, specSeq, specLive)
				return
			}
		case result := <-probeCh:
			probeCh = nil
			if result.err != nil || result.text == "" {
				break
			}
			probeAt, probeN, probeText = result.at, result.n, result.text
			u.policy.observeProgress(result.at, result.text)
			at := u.at(len(u.pcm))
			if !specLive && u.policy.voiced && !u.policy.inVoice && at-u.policy.lastVoicedAt >= endpointSpecQuiet {
				specSeq = c.t.nextTurn()
				specAt = at
				specLive = true
				speechMS := int64(0)
				if !speechAt.IsZero() {
					speechMS = speechAt.UnixMilli()
				}
				c.t.e.emit("turn.speculate", map[string]any{
					"terminal_id": c.t.id, "seq": specSeq, "transcript": result.text,
					"speech_at_ms": speechMS,
				})
			}
		case pkt := <-c.packets:
			if speechAt.IsZero() && u.policy.inVoice {
				speechAt = pkt.at
			}
			u.addFor(pkt.pcm, vad, c.listen)
			if !speechAt.IsZero() && u.policy.inVoice {
				speechAt = pkt.at
			}
			c.putSlot(pkt.pcm)
			at := u.at(len(u.pcm))
			silence := at - u.policy.lastVoicedAt
			if probeCh == nil && len(u.pcm) > 0 &&
				((u.policy.inVoice && at-probeAt >= endpointProbeEvery) ||
					(!u.policy.inVoice && u.policy.voiced && silence >= endpointSpecQuiet)) {
				launchProbe(len(u.pcm), at)
			}
			if specLive && u.at(len(u.pcm)) > specAt+endpointBytes {
				c.emitCancel(specSeq, "speech")
				specLive = false
			}
			if u.policy.inVoice {
				if speechAt.IsZero() {
					speechAt = pkt.at
				}
			}
			if d := classify(at); d.stops() {
				if d.discards() {
					c.emitCancel(specSeq, "noise")
				} else if probeText != "" && probeN == len(u.pcm) {
					c.commit(u, probeText, probeN, speechAt, specSeq, specLive)
				} else {
					text, _ := c.t.transcribe(c.ctx, u.clip(), false)
					c.commitText(text, u, speechAt, specSeq, specLive)
				}
				return
			}
			lastTick = pkt.at
		case <-time.After(20 * time.Millisecond):
			if len(c.packets) > 0 {
				continue
			}
			if lastTick.IsZero() {
				lastTick = time.Now()
			}
			at := u.at(len(u.pcm))
			if d := classify(at); d.stops() {
				text, _ := c.t.transcribe(c.ctx, u.clip(), false)
				if d.discards() {
					c.emitCancel(specSeq, "noise")
				} else {
					c.commitText(text, u, speechAt, specSeq, specLive)
				}
				return
			}
			_ = probeAt
		}
	}
}

func (c *captureState) transcribeCommit(u *utterance) string {
	text, err := c.t.transcribe(c.ctx, u.clip(), false)
	if err != nil {
		return ""
	}
	return text
}

func (c *captureState) commit(u *utterance, cached string, cachedN int, speechAt time.Time, seq uint64, spec bool) {
	text := cached
	if cachedN != len(u.pcm) || text == "" {
		text = c.transcribeCommit(u)
	}
	c.commitText(text, u, speechAt, seq, spec)
}

func (c *captureState) commitText(text string, u *utterance, speechAt time.Time, seq uint64, spec bool) {
	_ = spec
	if isFillerTranscript(text) {
		c.t.e.emit("turn.not_caught", map[string]any{"terminal_id": c.t.id})
		return
	}
	if seq == 0 {
		seq = c.t.nextTurn()
	}
	speechMS := int64(0)
	if !speechAt.IsZero() {
		speechMS = speechAt.UnixMilli()
	}
	c.t.e.emit("turn.commit", map[string]any{
		"terminal_id": c.t.id, "seq": seq, "transcript": text,
		"speech_at_ms": speechMS, "listen_window": c.listen,
		"audio_ms": pcmDuration(len(u.pcm)).Milliseconds(),
	})
}

func (c *captureState) emitCancel(seq uint64, reason string) {
	if seq == 0 {
		return
	}
	c.t.e.emit("turn.cancel", map[string]any{
		"terminal_id": c.t.id, "seq": seq, "reason": reason,
	})
}

func (c *captureState) finish() {
	if !c.device {
		op := uint8(proto.RecOpStop)
		if cancelReason(c.reason.Load()) == cancelUser {
			op = proto.RecOpCancel
		}
		c.t.ctrl(proto.MsgRecCtrl, proto.ProtoRecCtrl{Op: op, Session: c.session}.Encode())
	}
	if c.t.cap.CompareAndSwap(c, nil) {
		c.t.event(evCaptureDone, func(ev *termEvent) { ev.capture = c })
	}
	c.cancelFn()
}

func (c *captureState) runDevice() {
	var pcm []byte
	var progressAt time.Time
	start := time.Now()
	c.t.e.emit("capture.progress", map[string]any{
		"terminal_id": c.t.id, "capture_id": c.captureID, "remaining_s": c.seconds,
	})
	for {
		select {
		case <-c.ctx.Done():
			return
		case pkt := <-c.packets:
			pcm = append(pcm, pkt.pcm...)
			c.putSlot(pkt.pcm)
			if elapsed := time.Since(start); elapsed >= time.Duration(c.seconds)*time.Second {
				c.writeWAV(pcm)
				return
			}
			if time.Since(progressAt) >= time.Second {
				progressAt = time.Now()
				remaining := c.seconds - int(time.Since(start).Seconds())
				c.t.e.emit("capture.progress", map[string]any{
					"terminal_id": c.t.id, "capture_id": c.captureID, "remaining_s": max(0, remaining),
				})
			}
		case reason := <-c.cancelCh:
			_ = reason
			c.writeWAV(pcm)
			return
		}
	}
}

func (c *captureState) writeWAV(pcm []byte) {
	err := writeWAVAtomic(c.outPath, pcm)
	fields := map[string]any{
		"terminal_id": c.t.id, "capture_id": c.captureID, "path": c.outPath,
		"seconds": pcmDuration(len(pcm)).Seconds(),
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	c.t.e.emit("capture.done", fields)
}

func writeWAVAtomic(path string, pcm []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	h := audio.WavHeader(len(pcm))
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(h[:]); err == nil {
		_, err = f.Write(pcm)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

var errNoSTT = errors.New("STT unavailable")

func (t *Terminal) transcribe(ctx context.Context, pcm []byte, probe bool) (string, error) {
	stt := t.e.models.transcriber()
	if stt == nil {
		return "", errNoSTT
	}
	if probe {
		ctx = audio.WithProbe(ctx)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return stt.Transcribe(ctx, pcm)
}

func (t *Terminal) predict(pcm []byte) float32 {
	td := t.e.models.turnDetector()
	if td == nil || len(pcm) < sampleRate/2*2 {
		return -1
	}
	p, err := td.PredictPCM16(pcm)
	if err != nil {
		return -1
	}
	return p
}

func (t *Terminal) recStartEvent(m proto.ProtoRecStart) {
	if t.capture != nil {
		return
	}
	listen := m.Target == "listen"
	t.beginCapture(listen, 0, listen, "")
}

func (t *Terminal) recEndEvent(m proto.ProtoRecEnd) {
	if t.capture == nil {
		return
	}
	if m.Cancelled != 0 {
		t.capture.cancel(cancelUser)
	} else {
		t.capture.cancel(cancelSend)
	}
}

func (t *Terminal) startDeviceCapture(cmd *ipcCommand) {
	if t.capture != nil {
		t.capture.cancel(cancelUser)
	}
	c := newCapture(t, false, time.Duration(cmd.Seconds)*time.Second, "")
	c.device = true
	c.captureID = cmd.CaptureID
	c.outPath = cmd.OutPath
	c.seconds = cmd.Seconds
	t.capture = c
	t.cap.Store(c)
	t.mode = modeRecording
	go c.run()
}
