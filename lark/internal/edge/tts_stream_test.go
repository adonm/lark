package edge

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lark/internal/audio"
	proto "lark/internal/proto"
)

// streamFakeTTS emits pre-baked 24 kHz chunks through the progress API. gate,
// when non-nil, holds back every chunk after the first so a test can observe
// that audio left the edge before synthesis finished.
type streamFakeTTS struct {
	chunk   []float32
	chunks  int
	gate    chan struct{}
	delay   time.Duration
	emits   atomic.Int32
	batch   atomic.Bool
	finish  atomic.Bool
	started chan struct{}
}

func newStreamFakeTTS(chunks int, samples int) *streamFakeTTS {
	chunk := make([]float32, samples)
	for i := range chunk {
		chunk[i] = float32(0.3 * float64(1+i%7) / 7)
	}
	return &streamFakeTTS{chunk: chunk, chunks: chunks}
}

func (f *streamFakeTTS) Generate(string, audio.Voice, func() bool) ([]float32, int) {
	f.batch.Store(true)
	raw := make([]float32, 0, len(f.chunk)*f.chunks)
	for range f.chunks {
		raw = append(raw, f.chunk...)
	}
	return raw, audio.KokoroRate
}

func (f *streamFakeTTS) GenerateStream(_ string, _ audio.Voice, keepGoing func() bool, emit func([]float32) bool) (int, bool) {
	for i := 0; i < f.chunks; i++ {
		if !keepGoing() {
			return audio.KokoroRate, f.emits.Load() > 0
		}
		f.emits.Add(1)
		if !emit(f.chunk) {
			return audio.KokoroRate, true
		}
		if i == 0 && f.started != nil {
			close(f.started)
		}
		if i == 0 && f.gate != nil {
			select {
			case <-f.gate:
			case <-time.After(5 * time.Second):
				return audio.KokoroRate, true
			}
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
	}
	f.finish.Store(true)
	return audio.KokoroRate, true
}

func (f *streamFakeTTS) Close() {}

type streamTerminal struct {
	t   *Terminal
	s   *ttsStream
	nc  *nativeConn
	got []byte
}

func newStreamTerminal(synth Synthesizer) *streamTerminal {
	e := &Edge{models: &Models{tts: synth}, done: make(chan struct{})}
	t := &Terminal{
		deps:  terminalDeps{e: e},
		cache: newClipCache(4 << 20),
	}
	t.life.done = make(chan struct{})
	t.life.events = make(chan termEvent, 8)
	nc := &nativeConn{pcmOut: make(chan outMsg, 256)}
	t.conn.native.Store(nc)
	s := newTTSStream(t, 1, audio.Voice{ID: "af_heart", Lang: "en-us"})
	return &streamTerminal{t: t, s: s, nc: nc}
}

func (st *streamTerminal) nextPCM(t *testing.T, timeout time.Duration) []byte {
	t.Helper()
	select {
	case msg := <-st.nc.pcmOut:
		if msg.msgType != proto.MsgTTSPCM {
			t.Fatalf("message type = %d, want TTS PCM", msg.msgType)
		}
		return msg.payload
	case <-time.After(timeout):
		t.Fatal("timed out waiting for streamed PCM")
	}
	return nil
}

// The first chunk must leave the edge while the synthesizer is still blocked
// on a later chunk. A batch-only path would emit nothing until it returned.
func TestTTSStreamForwardsFirstChunkBeforeSynthesisEnds(t *testing.T) {
	fake := newStreamFakeTTS(3, 7200) // 300 ms per chunk at 24 kHz
	fake.gate = make(chan struct{})
	fake.started = make(chan struct{})
	st := newStreamTerminal(fake)
	go st.s.run()
	st.s.push("stream this long sentence")

	select {
	case <-fake.started:
	case <-time.After(2 * time.Second):
		t.Fatal("synthesizer never started")
	}
	first := st.nextPCM(t, 2*time.Second)
	if len(first) == 0 {
		t.Fatal("first PCM frame is empty")
	}
	select {
	case <-fake.gate:
	default:
	}
	// Synthesis is still gated, so this frame can only have come from the
	// first callback rather than a whole-utterance batch.
	if fake.finish.Load() {
		t.Fatal("synthesizer finished before the first chunk was observed")
	}

	fake.gate <- struct{}{}
	st.s.endInput()
	<-st.s.done
	all := append([]byte(nil), first...)
	for {
		select {
		case msg := <-st.nc.pcmOut:
			all = append(all, msg.payload...)
			continue
		default:
		}
		break
	}

	var raw []float32
	for range fake.chunks {
		raw = append(raw, fake.chunk...)
	}
	want, err := audio.SynthPCM(raw, audio.KokoroRate)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(all, want) {
		t.Fatalf("streamed PCM differs from batch synthesis: got %d bytes, want %d", len(all), len(want))
	}
}

func TestTTSStreamCancelStopsSynthesisMidStream(t *testing.T) {
	fake := newStreamFakeTTS(40, 7200)
	fake.delay = 20 * time.Millisecond
	st := newStreamTerminal(fake)
	go st.s.run()
	st.s.push("a long sentence that should be cut short")

	if got := st.nextPCM(t, 2*time.Second); len(got) == 0 {
		t.Fatal("first PCM frame is empty")
	}
	st.s.cancel()
	select {
	case <-st.s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop after cancel")
	}
	if fake.batch.Load() {
		t.Fatal("cancel fell back to full synthesis")
	}
	if n := fake.emits.Load(); n >= int32(fake.chunks) {
		t.Fatalf("synthesizer produced all %d chunks after cancel", n)
	}
}

func TestTTSStreamFallsBackWhenProgressUnavailable(t *testing.T) {
	fake := noProgressTTS{}
	st := newStreamTerminal(&fake)
	go st.s.run()
	st.s.push("fallback")
	st.s.endInput()
	<-st.s.done

	var got []byte
	for {
		select {
		case msg := <-st.nc.pcmOut:
			got = append(got, msg.payload...)
			continue
		default:
		}
		break
	}
	if !fake.batch.Load() {
		t.Fatal("batch Generate was not used after streaming failed")
	}
	if len(got) == 0 {
		t.Fatal("fallback produced no PCM")
	}
}

// noProgressTTS reports that streaming is unavailable and tracks the batch call.
type noProgressTTS struct{ batch atomic.Bool }

func (f *noProgressTTS) Generate(_ string, _ audio.Voice, _ func() bool) ([]float32, int) {
	f.batch.Store(true)
	raw := make([]float32, 4800)
	for i := range raw {
		raw[i] = 0.25
	}
	return raw, audio.KokoroRate
}

func (f *noProgressTTS) GenerateStream(string, audio.Voice, func() bool, func([]float32) bool) (int, bool) {
	return 0, false
}

func (f *noProgressTTS) Close() {}

func TestTTSStreamConcurrentCancelIsRaceFree(t *testing.T) {
	fake := newStreamFakeTTS(200, 4096)
	fake.delay = time.Millisecond
	st := newStreamTerminal(fake)
	go st.s.run()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			st.s.push("chunk")
		}
		st.s.endInput()
	}()
	time.Sleep(time.Duration(1+time.Now().UnixNano()%5) * time.Millisecond)
	st.s.cancel()
	select {
	case <-st.s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not stop")
	}
	wg.Wait()
}
