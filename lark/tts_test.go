package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKokoroReplyBypassesQueuedWarmup(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := &synthWorker{requests: make(chan ttsRequest, 1), warming: make(chan ttsRequest, 2), done: make(chan struct{})}
	started := make(chan string, 3)
	release := make(chan struct{})
	go w.run(ctx, func() (speechEngine, error) {
		return fakeSpeechEngine(func(text string, _ voiceChoice, _ func() bool) ([]float32, int) {
			started <- text
			if text == "first warm clip" {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return []float32{0, 0.2, 0}, kokoroRate
		}), nil
	})
	request := func(text string) ttsRequest {
		return ttsRequest{ctx: ctx, text: text, queuedAt: time.Now(), done: make(chan ttsResult, 1)}
	}
	w.warming <- request("first warm clip")
	expectValue(t, started, "first warm clip")
	w.warming <- request("second warm clip")
	w.requests <- request("reply")
	close(release)
	expectValue(t, started, "reply")
	expectValue(t, started, "second warm clip")
}

func TestFirstChunkAndCachedOpener(t *testing.T) {
	t.Parallel()
	ex := sentenceExtractor{firstChunk: true}
	var got []string
	for _, d := range []string{"Two plus ", "two is ", "four ", "right now."} {
		got = append(got, ex.add(d)...)
	}
	got = append(got, ex.flush()...)
	if len(got) < 2 || got[0] != "Two plus two is four" {
		t.Fatalf("expected 5-word first chunk, got %q", got)
	}

	ex2 := sentenceExtractor{firstChunk: true}
	got2 := ex2.add("Good question, let me think about that.")
	got2 = append(got2, ex2.flush()...)
	if len(got2) < 2 || got2[0] != "Good question, let me think" {
		t.Fatalf("expected first chunk split, got %q", got2)
	}

	var clips speechClips
	clips.clips = map[clipKey]*clip{
		{heartVoice, "Sure!"}: {done: make(chan struct{}), pcm: []byte{1, 2, 3, 4}},
	}
	close(clips.clips[clipKey{heartVoice, "Sure!"}].done)
	pcm, rest, ok := splitCachedOpener(&clips, heartVoice, "Sure! I can help.")
	if !ok || len(pcm) != 4 || rest != "I can help." {
		t.Fatalf("unexpected cached opener split: ok=%v len=%d rest=%q", ok, len(pcm), rest)
	}
}

func TestSpeechTurnUsesPreSynthesizedFirstChunk(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clips := &speechClips{clips: map[clipKey]*clip{
		{heartVoice, "A fresh answer"}: {done: make(chan struct{}), pcm: []byte{7, 8}},
	}}
	close(clips.clips[clipKey{heartVoice, "A fresh answer"}].done)
	var calls atomic.Int32
	w := fakeTTS(ctx, &calls)
	turn := newSpeechTurn(ctx, func() {}, w, clips, heartVoice, nil)
	if !turn.complete([]string{"A fresh answer"}) {
		t.Fatal("speech turn rejected its final text")
	}
	first, err := turn.stream.chunk(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, []byte{7, 8}) {
		t.Fatalf("first chunk was re-synthesized: %v", first)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("cached first chunk still queued %d TTS jobs", n)
	}
}

func TestSpeechTurnAwaitsInFlightFirstChunk(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	clips := &speechClips{}
	var calls atomic.Int32
	w := fakeTTS(ctx, &calls)
	item, owner := clips.reserve(heartVoice, "A fresh answer")
	if !owner {
		t.Fatal("reserve did not claim a new clip")
	}
	go clips.fill(ctx, w, heartVoice, "A fresh answer", item)
	turn := newSpeechTurn(ctx, func() {}, w, clips, heartVoice, nil)
	if !turn.complete([]string{"A fresh answer"}) {
		t.Fatal("speech turn rejected its final text")
	}
	first, err := turn.stream.chunk(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first, []byte{1, 2, 3, 4}) {
		t.Fatalf("unexpected first chunk: %v", first)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("in-flight first chunk synthesized %d times, want 1", n)
	}
}

func TestSpeculativeTurnHitAndAbort(t *testing.T) {
	t.Parallel()
	var prompts []string
	var mu sync.Mutex
	promptSeen := make(chan struct{})
	b, _ := wsHarness(t, func(b *larkServer, dir string) {
		b.worker = fakeTTS(b.root, nil)
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/event":
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": connected\n\n")
				flusher := w.(http.Flusher)
				flusher.Flush()
				select {
				case <-promptSeen:
					_, _ = io.WriteString(w, `data: {"type":"session.text.delta","data":{"sessionID":"ses-spec","delta":"Speculative answer! "}}`+"\n\n")
					_, _ = io.WriteString(w, `data: {"type":"session.execution.succeeded","data":{"sessionID":"ses-spec"}}`+"\n\n")
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
				<-r.Context().Done()
			case r.Method == http.MethodPost && r.URL.Path == "/api/session":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-spec"}}`))
			case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/instructions/entries/"):
				w.WriteHeader(204)
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses-spec":
				_, _ = w.Write([]byte(`{"data":{"id":"ses-spec"}}`))
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/model"):
				w.WriteHeader(204)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interrupt"):
				w.WriteHeader(204)
			case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses-spec/prompt":
				var body struct{ Text string }
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				prompts = append(prompts, body.Text)
				n := len(prompts)
				mu.Unlock()
				if n == 1 {
					close(promptSeen)
				}
				_, _ = fmt.Fprintf(w, `{"data":{"id":"p-%d"}}`, n)
			case r.Method == http.MethodPost && r.URL.Path == "/api/experimental/session/ses-spec/wait":
				w.WriteHeader(204)
			case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses-spec/message":
				_, _ = w.Write([]byte(`{"data":[{"id":"a-1","type":"assistant","finish":"stop","content":[{"type":"text","text":"Speculative answer!"}]},{"id":"p-1","type":"user"}]}`))
			default:
				w.WriteHeader(404)
			}
		})
	})
	comp := b.companionFor("cake")
	b.oc.feed.sig.wait(t, func() bool {
		b.oc.feed.mu.Lock()
		defer b.oc.feed.mu.Unlock()
		return b.oc.feed.connected
	})

	// 1. Promoted speculative turn delivers reply, audio, and records history once.
	hit := b.startSpeculate("cake", "What is two plus two?", 200, "voice", turnChannel{})
	deadline := time.Now().Add(testWait(t))
	for len(b.clips.peek(b.voiceFor("cake"), "Speculative answer!")) == 0 {
		if time.Now().After(deadline) {
			hit.mu.Lock()
			state := fmt.Sprintf("aborted=%v promoted=%v warming=%v firstAt=%v sentences=%q result=%d",
				hit.aborted, hit.promoted, hit.warming, hit.firstAt, hit.sentences, len(hit.done))
			hit.mu.Unlock()
			t.Fatalf("speculative turn did not pre-synthesize its first sentence: %s", state)
		}
		time.Sleep(5 * time.Millisecond)
	}
	res, err := b.generateReply(t.Context(), b.root, comp, "What is two plus two?", turnOptions{
		source:  "voice",
		channel: turnChannel{spec: hit},
		spec:    hit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.reply != "Speculative answer!" || res.audio == nil {
		t.Fatalf("unexpected promoted spec result: %+v", res)
	}
	entries, err := b.history.list(t.Context(), "cake")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 history entries (user + assistant) for promoted spec turn, got %d: %+v", len(entries), entries)
	}

	// 2. Aborted speculative turn never writes to history.
	aborted := b.startSpeculate("mint", "Set a", 100, "voice", turnChannel{})
	aborted.abort()
	entries, err = b.history.list(t.Context(), "mint")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("aborted speculative turn polluted history: %+v", entries)
	}
	_ = slices.Clone(entries)
	_ = context.Canceled
}

func TestSpeechSilenceChunkIsShared(t *testing.T) {
	t.Parallel()
	s := newAudioStream()
	if !s.appendSilence(sampleRate/5) || !s.append([]byte{1, 2}) {
		t.Fatal("stream rejected chunks")
	}
	if &s.chunks[0][0] != &speechPause[0] {
		t.Fatal("silence was copied instead of referencing the shared chunk")
	}
}
