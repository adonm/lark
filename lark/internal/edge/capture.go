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

type smartTurnRequest struct {
	at  time.Duration
	pcm []byte
}

type smartTurnResult struct {
	at   time.Duration
	prob float32
}

// Smart Turn only looks at the last eight seconds, so the async predictor
// copies just that tail instead of the whole utterance.
const smartTurnWindowBytes = sampleRate * 2 * 8

// probeScratch grows one clip across probes instead of allocating a fresh
// utterance copy every 250 ms. Only one probe runs at a time, and a new fill
// happens after the previous result arrives, so the worker never reads a
// buffer that is being rewritten. The generation catches the listening
// pre-roll reset that restarts the utterance buffer.
type probeScratch struct {
	buf []byte
	gen uint64
	n   int
}

func (p *probeScratch) fill(pcm []byte, n int, gen uint64) []byte {
	if gen != p.gen || n < p.n || len(p.buf) != p.n {
		p.buf = append(p.buf[:0], pcm[:n]...)
	} else if n > p.n {
		p.buf = append(p.buf, pcm[p.n:n]...)
	}
	p.gen, p.n = gen, n
	return p.buf
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
	if active := t.capture.active.Load(); active != nil {
		active.cancel(cancelUser)
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
	if t.info.caps&proto.CapEdgeEndpoint != 0 {
		flags |= proto.RecTargetEdgeE
	}
	if listen {
		flags |= proto.RecTargetListen
	}
	t.ctrl(proto.MsgRecCtrl, proto.ProtoRecCtrl{Op: proto.RecOpStart, Flags: flags, Session: c.session}.Encode())
	t.capture.active.Store(c)
	t.capture.pending.Store(c)
	t.screen.mode = modeListening
	if !listen {
		t.screen.mode = modeRecording
	}
	go c.run()
}

func (t *Terminal) ingestRTP(pcm []byte, at time.Time) {
	c := t.capture.pending.Load()
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
	vad := c.t.deps.e.models.newVAD()
	if vad == nil {
		vad = &energyVAD{}
	}
	u := &utterance{pcm: make([]byte, 0, utteranceBytes(c.limit))}
	var (
		probeCh       <-chan probeResult
		probeAt       time.Duration
		probeLaunchAt time.Duration
		probeN        int
		probeText     string
		scratch       probeScratch
		specSeq       uint64
		specAt        time.Duration
		specLive      bool
		specCount     int
		// smartProb belongs to smartProbAt; -1 means no prediction for the
		// current pause yet. A busy worker keeps its buffer untouched until the
		// result returns, so predicting never races the RTP packet loop.
		smartProb   float32       = -1
		smartProbAt time.Duration = -1
		smartBusy   bool
		smartBuf    []byte
		speechAt    time.Time
		lastTick    time.Time
	)
	probeCtx, cancelProbes := context.WithCancel(c.ctx)
	defer cancelProbes()

	// STT and Smart Turn run off the packet loop. Reusing one probe buffer is
	// safe because the result channel gates the next launch: the goroutine has
	// finished reading before the capture goroutine writes a new clip.
	launchProbe := func(n int, at time.Duration) {
		clip := scratch.fill(u.pcm, n, u.gen)
		ch := make(chan probeResult, 1)
		probeCh = ch
		ctx := probeCtx
		go func() {
			text, err := c.t.transcribe(ctx, clip, true)
			ch <- probeResult{at: at, n: n, text: text, err: err}
		}()
	}

	smartReq := make(chan smartTurnRequest, 1)
	smartRes := make(chan smartTurnResult, 1)
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			case req := <-smartReq:
				prob := c.t.predict(req.pcm)
				select {
				case smartRes <- smartTurnResult{at: req.at, prob: prob}:
				case <-c.ctx.Done():
					return
				}
			}
		}
	}()

	requestSmartTurn := func() {
		if smartBusy {
			return
		}
		if len(u.pcm) > smartTurnWindowBytes {
			smartBuf = append(smartBuf[:0], u.pcm[len(u.pcm)-smartTurnWindowBytes:]...)
		} else {
			smartBuf = append(smartBuf[:0], u.pcm...)
		}
		select {
		case smartReq <- smartTurnRequest{at: u.policy.lastVoicedAt, pcm: smartBuf}:
			smartBusy = true
		default:
		}
	}

	classify := func(at time.Duration) decision {
		if u.policy.voiced && !u.policy.inVoice {
			silence := at - u.policy.lastVoicedAt
			if silence >= endpointSpecQuiet && smartProbAt != u.policy.lastVoicedAt {
				requestSmartTurn()
			}
		}
		prob := float32(-1)
		if smartProbAt == u.policy.lastVoicedAt {
			prob = smartProb
		}
		return u.policy.classify(at, endpointCap+c.grace, c.limit, prob, c.listen)
	}

	poll := time.NewTimer(endpointFrame)
	defer poll.Stop()
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
				c.commit(u, probeText, probeN, speechAt, specSeq, specLive, specCount)
				return
			}
		case result := <-smartRes:
			smartBusy = false
			smartProbAt = result.at
			smartProb = result.prob
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
				specCount++
				speechMS := int64(0)
				if !speechAt.IsZero() {
					speechMS = speechAt.UnixMilli()
				}
				c.t.deps.e.emit("turn.speculate", map[string]any{
					"terminal_id": c.t.info.id, "seq": specSeq, "transcript": result.text,
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
			// Trailing silence needs one probe that covers every voiced frame;
			// re-probing silence at the packet rate both saturates Parakeet and
			// cannot improve the transcript. probeAt only advances on a
			// successful transcript, so throttle failed/empty probes too.
			quietProbe := !u.policy.inVoice && u.policy.voiced && silence >= endpointSpecQuiet &&
				probeAt < u.policy.lastVoicedAt &&
				(probeLaunchAt <= u.policy.lastVoicedAt || at-probeLaunchAt >= endpointProbeEvery)
			probeDue := probeCh == nil && len(u.pcm) > 0 &&
				((u.policy.inVoice && at-probeLaunchAt >= endpointProbeEvery) || quietProbe)
			if probeDue {
				launchProbe(len(u.pcm), at)
				probeLaunchAt = at
			}
			// A speculation only dies when speech actually resumes; comparing
			// durations against endpointBytes cancelled it during silence.
			if specLive && u.policy.inVoice && at > specAt+endpointFrame {
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
					c.commit(u, probeText, probeN, speechAt, specSeq, specLive, specCount)
				} else {
					start := time.Now()
					text, _ := c.t.transcribe(c.ctx, u.clip(), false)
					c.commitText(text, u, speechAt, specSeq, specLive, specCount, time.Since(start))
				}
				return
			}
			lastTick = pkt.at
		case <-poll.C:
			if !poll.Stop() {
				select {
				case <-poll.C:
				default:
				}
			}
			poll.Reset(endpointFrame)
			if len(c.packets) > 0 {
				continue
			}
			// The policy clock normally advances with received audio. When RTP
			// stops, the packet path can no longer move it; fall back to wall
			// time so endpointing (and the idle discard) still fires instead of
			// leaving the turn open until the window limit.
			now := time.Now()
			if lastTick.IsZero() {
				lastTick = now
			}
			at := u.at(len(u.pcm)) + max(0, now.Sub(lastTick))
			// Without RTP no VAD frame can end the voice, so the wall clock
			// has to expire it or the turn stays open for the whole window.
			if u.policy.inVoice && at-u.policy.lastVoicedAt >= endpointFrame {
				u.policy.inVoice = false
			}
			if d := classify(at); d.stops() {
				if d.discards() {
					c.emitCancel(specSeq, "noise")
				} else {
					start := time.Now()
					text, _ := c.t.transcribe(c.ctx, u.clip(), false)
					c.commitText(text, u, speechAt, specSeq, specLive, specCount, time.Since(start))
				}
				return
			}
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

func (c *captureState) commit(u *utterance, cached string, cachedN int, speechAt time.Time, seq uint64, spec bool, specs int) {
	text := cached
	var stt time.Duration
	if cachedN != len(u.pcm) || text == "" {
		start := time.Now()
		text = c.transcribeCommit(u)
		stt = time.Since(start)
	}
	c.commitText(text, u, speechAt, seq, spec, specs, stt)
}

func (c *captureState) commitText(text string, u *utterance, speechAt time.Time, seq uint64, spec bool, specs int, stt time.Duration) {
	_ = spec
	if isFillerTranscript(text) {
		c.t.deps.e.emit("turn.not_caught", map[string]any{"terminal_id": c.t.info.id})
		return
	}
	if seq == 0 {
		seq = c.t.nextTurn()
	}
	speechMS := int64(0)
	if !speechAt.IsZero() {
		speechMS = speechAt.UnixMilli()
	}
	audioMS := pcmDuration(len(u.pcm)).Milliseconds()
	c.t.deps.e.logf("turn.commit terminal=%s audio_ms=%d spec=%d stt_ms=%d",
		c.t.info.id, audioMS, specs, stt.Milliseconds())
	c.t.deps.e.emit("turn.commit", map[string]any{
		"terminal_id": c.t.info.id, "seq": seq, "transcript": text,
		"speech_at_ms": speechMS, "listen_window": c.listen,
		"audio_ms": audioMS,
	})
}

func (c *captureState) emitCancel(seq uint64, reason string) {
	if seq == 0 {
		return
	}
	c.t.deps.e.emit("turn.cancel", map[string]any{
		"terminal_id": c.t.info.id, "seq": seq, "reason": reason,
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
	if c.t.capture.pending.CompareAndSwap(c, nil) {
		c.t.event(evCaptureDone, func(ev *termEvent) { ev.capture = c })
	}
	c.cancelFn()
}

func (c *captureState) runDevice() {
	var pcm []byte
	var progressAt time.Time
	start := time.Now()
	c.t.deps.e.emit("capture.progress", map[string]any{
		"terminal_id": c.t.info.id, "capture_id": c.captureID, "remaining_s": c.seconds,
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
				c.t.deps.e.emit("capture.progress", map[string]any{
					"terminal_id": c.t.info.id, "capture_id": c.captureID, "remaining_s": max(0, remaining),
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
		"terminal_id": c.t.info.id, "capture_id": c.captureID, "path": c.outPath,
		"seconds": pcmDuration(len(pcm)).Seconds(),
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	c.t.deps.e.emit("capture.done", fields)
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
	stt := t.deps.e.models.transcriber()
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
	td := t.deps.e.models.turnDetector()
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
	if t.capture.active.Load() != nil {
		return
	}
	listen := m.Target == "listen"
	t.beginCapture(listen, 0, listen, "")
}

func (t *Terminal) recEndEvent(m proto.ProtoRecEnd) {
	active := t.capture.active.Load()
	if active == nil {
		return
	}
	if m.Cancelled != 0 {
		active.cancel(cancelUser)
	} else {
		active.cancel(cancelSend)
	}
}

func (t *Terminal) startDeviceCapture(cmd *ipcCommand) {
	if active := t.capture.active.Load(); active != nil {
		active.cancel(cancelUser)
	}
	c := newCapture(t, false, time.Duration(cmd.Seconds)*time.Second, "")
	c.device = true
	c.captureID = cmd.CaptureID
	c.outPath = cmd.OutPath
	c.seconds = cmd.Seconds
	t.capture.active.Store(c)
	t.capture.pending.Store(c)
	t.screen.mode = modeRecording
	go c.run()
}
