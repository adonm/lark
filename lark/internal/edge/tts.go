package edge

import (
	"container/list"
	"context"
	"sync"
	"time"

	"lark/internal/audio"
	proto "lark/internal/proto"
)

const (
	pcmPacket      = 4 << 10
	ttsPrefill     = 250 * time.Millisecond
	ttsPrefillByte = sampleRate * 2 * int(ttsPrefill/time.Second)
)

// clipCache is a bounded LRU of synthesized PCM keyed by voice and text.
// Pinned entries (fillers, openers, previews) are never evicted.
type clipCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[string]*list.Element
}

type clipEntry struct {
	key    string
	pcm    []byte
	pinned bool
}

func newClipCache(max int) *clipCache {
	return &clipCache{max: max, order: list.New(), items: make(map[string]*list.Element)}
}

func (c *clipCache) Get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.items[key]
	if el == nil {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*clipEntry).pcm, true
}

func (c *clipCache) Put(key string, pcm []byte, pinned bool) {
	if c == nil || len(pcm) == 0 || len(pcm) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el := c.items[key]; el != nil {
		e := el.Value.(*clipEntry)
		e.pcm, e.pinned = pcm, e.pinned || pinned
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&clipEntry{key: key, pcm: pcm, pinned: pinned})
	c.items[key] = el
	c.size += len(pcm)
	for c.size > c.max {
		victim := c.oldestUnpinned()
		if victim == nil {
			break
		}
		c.remove(victim)
	}
}

func (c *clipCache) oldestUnpinned() *list.Element {
	for el := c.order.Back(); el != nil; el = el.Prev() {
		if !el.Value.(*clipEntry).pinned {
			return el
		}
	}
	return nil
}

func (c *clipCache) remove(el *list.Element) {
	e := el.Value.(*clipEntry)
	delete(c.items, e.key)
	c.size -= len(e.pcm)
	c.order.Remove(el)
}

func clipKey(voice, text string) string { return voice + "\x00" + text }

// ttsStream is one playback. Chunks arrive from the brain; a single goroutine
// synthesizes, caches and queues PCM in order.
type ttsStream struct {
	t     *Terminal
	id    uint64
	voice audio.Voice
	texts chan string
	end   chan struct{}
	ctx   context.Context
	stop  context.CancelFunc
	done  chan struct{}

	once sync.Once
}

func newTTSStream(t *Terminal, id uint64, voice audio.Voice) *ttsStream {
	ctx, cancel := context.WithCancel(context.Background())
	return &ttsStream{
		t: t, id: id, voice: voice,
		texts: make(chan string, 64), end: make(chan struct{}),
		ctx: ctx, stop: cancel, done: make(chan struct{}),
	}
}

func (s *ttsStream) push(text string) {
	if text == "" {
		return
	}
	select {
	case s.texts <- text:
	case <-s.ctx.Done():
	case <-s.done:
	}
}

func (s *ttsStream) endInput() { s.once.Do(func() { close(s.end) }) }

func (s *ttsStream) cancel() { s.stop() }

func (s *ttsStream) run() {
	defer close(s.done)
	var scratch []float32
	pending := make([]byte, 0, ttsPrefillByte+2*pcmPacket)
	sent := false
	flush := func() {
		for len(pending) > 0 {
			n := min(pcmPacket, len(pending))
			if !s.t.pcm(pending[:n]) {
				s.stop()
				return
			}
			if !sent {
				sent = true
			}
			pending = pending[n:]
		}
		pending = pending[:0]
	}
	synth := func(text string) {
		pcm := s.synthesize(&scratch, text)
		if len(pcm) == 0 {
			return
		}
		pending = append(pending, pcm...)
		if len(pending) >= ttsPrefillByte {
			flush()
		}
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case text := <-s.texts:
			synth(text)
		case <-s.end:
			for {
				select {
				case text := <-s.texts:
					synth(text)
					continue
				default:
				}
				break
			}
			flush()
			s.t.event(evTTSStreamDone, func(ev *termEvent) { ev.tts = s })
			return
		}
	}
}

func (s *ttsStream) synthesize(scratch *[]float32, text string) []byte {
	key := clipKey(s.voice.ID, text)
	if pcm, ok := s.t.cache.Get(key); ok {
		return pcm
	}
	tts := s.t.e.models.synthesizer()
	if tts == nil {
		return nil
	}
	raw, rate := tts.Generate(text, s.voice, func() bool { return s.ctx.Err() == nil })
	if len(raw) == 0 {
		return nil
	}
	pcm, next, err := audio.SynthPCMWithScratch(raw, rate, *scratch)
	*scratch = next
	if err != nil {
		return nil
	}
	s.t.cache.Put(key, pcm, false)
	return pcm
}

func (t *Terminal) speakStart(ev termEvent) {
	if ev.c == nil {
		return
	}
	if t.tts != nil {
		t.tts.cancel()
		t.tts = nil
	}
	voice := t.voice(ev.c.Voice)
	id := ev.c.StreamID
	if id == 0 {
		id = t.e.nextStream()
	}
	s := newTTSStream(t, id, voice)
	t.tts = s
	t.awaitAck = 0
	t.mode = modeSpeaking
	t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStart, AudioID: id}.Encode())
	go s.run()
}

func (t *Terminal) playEphemeral(voiceID, text string, pinned bool) {
	voice := t.voice(voiceID)
	id := t.e.nextStream()
	s := newTTSStream(t, id, voice)
	if pcm, ok := t.cache.Get(clipKey(voice.ID, text)); ok && len(pcm) > 0 {
		t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStart, AudioID: id}.Encode())
		for len(pcm) > 0 {
			n := min(pcmPacket, len(pcm))
			t.pcm(pcm[:n])
			pcm = pcm[n:]
		}
		t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpEnd, AudioID: id}.Encode())
		t.awaitAck = id
		return
	}
	t.tts = s
	t.awaitAck = 0
	t.mode = modeSpeaking
	t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStart, AudioID: id}.Encode())
	go func() {
		pcm := s.synthesize(new([]float32), text)
		if len(pcm) > 0 {
			t.cache.Put(clipKey(voice.ID, text), pcm, pinned)
			s.pendingSend(pcm)
		}
		s.eventDone()
	}()
}

func (s *ttsStream) pendingSend(pcm []byte) {
	for len(pcm) > 0 {
		n := min(pcmPacket, len(pcm))
		if !s.t.pcm(pcm[:n]) {
			return
		}
		pcm = pcm[n:]
	}
}

func (s *ttsStream) eventDone() {
	s.t.event(evTTSStreamDone, func(ev *termEvent) { ev.tts = s })
}

// pcmStream plays generated PCM (metronome or tune) over the same TTS start /
// PCM / end control path as speech. It paces chunks against a small lead so a
// long metronome cannot overflow the queue, while Stop cancels immediately.
type pcmStream struct {
	t    *Terminal
	id   uint64
	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
	once sync.Once
}

func (s *pcmStream) cancel() { s.once.Do(s.stop) }

func (t *Terminal) startPCMStream(id uint64, produce func(context.Context) <-chan []byte) *pcmStream {
	if t.tts != nil {
		t.tts.cancel()
		t.tts = nil
	}
	if t.pcmPlay != nil {
		t.pcmPlay.cancel()
		t.pcmPlay = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &pcmStream{t: t, id: id, ctx: ctx, stop: cancel, done: make(chan struct{})}
	t.pcmPlay = s
	t.awaitAck = 0
	t.mode = modeSpeaking
	t.ordered(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStart, AudioID: id}.Encode())
	go s.run(produce(ctx))
	return s
}

func (s *pcmStream) run(chunks <-chan []byte) {
	defer close(s.done)
	const lead = 200 * time.Millisecond
	started := time.Now()
	sent := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case chunk, ok := <-chunks:
			if !ok {
				s.t.event(evPCMStreamDone, func(ev *termEvent) { ev.pcmPlay = s })
				return
			}
			target := started.Add(time.Duration(sent) * time.Second / sampleRate)
			if d := time.Until(target.Add(-lead)); d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-s.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			if !s.t.pcm(chunk) {
				s.cancel()
				return
			}
			sent += len(chunk) / 2
		}
	}
}

func (t *Terminal) playMetronome(id uint64, spec audio.MetronomeSpec) {
	t.startPCMStream(id, func(ctx context.Context) <-chan []byte {
		chunks, err := audio.MetronomeStream(ctx, spec, 2048)
		if err != nil {
			closed := make(chan []byte)
			close(closed)
			return closed
		}
		return chunks
	})
}

func (t *Terminal) playTune(id uint64, pcm []byte) {
	t.startPCMStream(id, func(ctx context.Context) <-chan []byte {
		return chunkPCM(ctx, pcm, 2048)
	})
}

// chunkPCM slices an already-rendered buffer without copying it. The producer
// honours ctx so a tune stops on the same tap as speech.
func chunkPCM(ctx context.Context, pcm []byte, size int) <-chan []byte {
	if size <= 0 {
		size = 2048
	}
	out := make(chan []byte)
	go func() {
		defer close(out)
		for len(pcm) > 0 {
			n := min(size*2, len(pcm))
			chunk := pcm[:n]
			pcm = pcm[n:]
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (t *Terminal) handleAck(m proto.ProtoTTSAck) {
	if t.awaitAck == 0 || m.AudioID != t.awaitAck {
		return
	}
	id := t.awaitAck
	t.awaitAck = 0
	if t.mode == modeSpeaking {
		t.mode = modeIdle
	}
	t.e.emit("playback.done", map[string]any{
		"terminal_id": t.id, "stream_id": id, "aborted": m.Aborted != 0,
		"underruns_delta": 0,
	})
}

func (t *Terminal) stopAudio(aborted bool) {
	if t.tts != nil {
		t.tts.cancel()
		t.tts = nil
	}
	var pcmID uint64
	if t.pcmPlay != nil {
		pcmID = t.pcmPlay.id
		t.pcmPlay.cancel()
		t.pcmPlay = nil
	}
	id := t.awaitAck
	t.awaitAck = 0
	t.mode = modeIdle
	if pcmID != 0 {
		t.ctrl(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStop, AudioID: pcmID}.Encode())
		t.e.emit("playback.done", map[string]any{
			"terminal_id": t.id, "stream_id": pcmID, "aborted": aborted,
			"underruns_delta": 0,
		})
	}
	if id != 0 {
		t.ctrl(proto.MsgTTSCtrl, proto.ProtoTTSCtrl{Op: proto.TtsOpStop, AudioID: id}.Encode())
		t.e.emit("playback.done", map[string]any{
			"terminal_id": t.id, "stream_id": id, "aborted": aborted,
			"underruns_delta": 0,
		})
	}
}
