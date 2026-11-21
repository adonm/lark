package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

func TestTurnPromptIdentifiesDeviceAndInput(t *testing.T) {
	for _, tc := range []struct {
		id, source, kind string
		web              bool
	}{
		{"lark-33d8", "voice", "hardware", false},
		{"virt-1234abcd", "typed", "web", false},
		{"phone-1234", "tap", "web", false},
		{"other-id", "tap", "web", true},
	} {
		if got := turnPrompt(peer{DeviceID: tc.id}, tc.web, tc.source, "Hi", companionState{companionLook: companionLook{Name: "Cake", Configured: true}}); got !=
			"[Cake ("+tc.id+") · "+tc.kind+" · "+tc.source+"]\nHi" {
			t.Fatal(got)
		}
	}
}

func TestSentencesAndFence(t *testing.T) {
	want := []string{"Whoa!", "Dr. Lee found a 3.2-inch shell.", "Ooh, awesome question! What made it spiral?"}
	if got := SplitSentences("**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral?"); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	var s sentenceExtractor
	var streamed []string
	for _, delta := range []string{"**Whoa!** Dr. Lee found a 3.", "2-inch shell. ", "Ooh, awesome question!", " What made it spiral in circles?\n", "``", "`lark-screen", "\nNever say this. \n```", "Not eager even after fence. "} {
		streamed = append(streamed, s.add(delta)...)
	}
	want[2] = "Ooh, awesome question! What made it spiral in circles?"
	if !s.halted || !slices.Equal(streamed, want) {
		t.Fatal(streamed)
	}
}

func TestSentenceExtractorFragmentation(t *testing.T) {
	for _, text := range []string{
		"**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral? ",
		"\t A. B. Wood and Prof. Façade saw it…\u2002Then it returned.\u00a0“Really?” No way. ",
		"What happened at St. James? “Time to see the world!” she said. “Wait.”...? unfinished",
		"`inline` and ``more`` markup. Yes.```svg\n This is never read. ```",
		"NotMr. NotProf. e.g. a note. A.B. This can finish. _",
	} {
		raw, _, fence := strings.Cut(text, "```")
		unmarked := strings.NewReplacer("*", "", "_", "", "`", "", "#", "").Replace(raw)
		want := sentences(plainText(raw), false, fence || unmarked != strings.TrimRightFunc(unmarked, unicode.IsSpace))
		for size := 1; size <= len(text); size++ {
			var extractor sentenceExtractor
			var got []string
			for i := 0; i < len(text); i += size {
				got = append(got, extractor.add(text[i:min(i+size, len(text))])...)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%q in %d-byte chunks: got %q, want %q", text, size, got, want)
			}
		}
	}
}

func BenchmarkSentenceExtractor(b *testing.B) {
	text := strings.Repeat("**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral? ", 25)
	for name, size := range map[string]int{"single": len(text), "fragmented": 5} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				var extractor sentenceExtractor
				for i := 0; i < len(text); i += size {
					extractor.add(text[i:min(i+size, len(text))])
				}
			}
		})
	}
}

func sineFloats(n, hz int, gain float64) []float32 {
	raw := make([]float32, n)
	for i := range n {
		raw[i] = float32(gain * math.Sin(2*math.Pi*float64(hz*i)/24000))
	}
	return raw
}

func TestAudioDSP(t *testing.T) {
	rms := func(pcm []byte) float64 {
		var sum float64
		for i := 0; i < len(pcm); i += 2 {
			v := float64(int16(le.Uint16(pcm[i:])))
			sum += v * v
		}
		return math.Sqrt(sum / float64(len(pcm)/2))
	}
	pcm, err := synthPCM(sineFloats(24000, 1000, .25), 24000)
	if err != nil || len(pcm) != 32000 || math.Abs(rms(pcm)-10426) > 100 {
		t.Fatalf("resampler passband: %d bytes, error %v", len(pcm), err)
	}
	alias, err := synthPCM(sineFloats(24000, 10000, .25), 24000)
	if err != nil || rms(alias[64:len(alias)-64]) > 110 {
		t.Fatalf("resampler aliasing: %v", err)
	}
	loud := bytes.Repeat([]byte{0x80, 0x3e}, 60)
	quiet := bytes.Repeat([]byte{64, 0}, 120)
	normalized := int16(le.Uint16(NormalizeVoicePCM(loud)))
	if normalized < 25000 || normalized > 26100 || !bytes.Equal(NormalizeVoicePCM(quiet), quiet) {
		t.Fatal("bad normalization or noise-floor guard")
	}

}

func BenchmarkSincOneSecond(b *testing.B) {
	raw := sineFloats(24000, 1000, .25)
	for b.Loop() {
		_, _ = synthPCM(raw, 24000)
	}
}

func TestAudioStream(t *testing.T) {
	b := &bridge{token: "unit"}
	srv := httptest.NewServer(b)
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second
	first, last := bytes.Repeat([]byte{1, 0}, 32), bytes.Repeat([]byte{2, 0}, 32)
	open := func(id string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/audio/"+id, nil)
		req.Header.Set("X-Lark-Token", "unit")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, fail := range []bool{false, true} {
		s := newAudioStream()
		woke := make(chan bool, 1)
		go func() { chunk, err := s.chunk(t.Context(), 0); woke <- err == nil && bytes.Equal(chunk, first) }()
		if !s.append(first) || !<-woke {
			t.Fatal("first chunk did not wake reader")
		}
		id, size, streaming := b.audio.add(s)
		if id == "" || size != 0 || !streaming {
			t.Fatal("not streaming")
		}
		res := open(id)
		if res.StatusCode != 200 || !slices.Contains(res.TransferEncoding, "chunked") {
			t.Fatal("missing chunked framing")
		}
		got := make([]byte, len(first))
		if _, err := io.ReadFull(res.Body, got); err != nil || !bytes.Equal(got, first) {
			t.Fatalf("first chunk: %v", err)
		}
		if fail {
			s.finish(errTTS)
		} else {
			s.append(last)
			s.finish(nil)
		}
		tail, err := io.ReadAll(res.Body)
		res.Body.Close()
		replay := open(id)
		pcm, replayErr := io.ReadAll(replay.Body)
		replay.Body.Close()
		if fail && (err != io.ErrUnexpectedEOF || replay.StatusCode != 404) {
			t.Fatalf("failed stream persisted or looked complete: %v HTTP %d", err, replay.StatusCode)
		}
		if !fail && (err != nil || replayErr != nil || !bytes.Equal(tail, last) || replay.ContentLength != int64(len(first)+len(last)) ||
			!bytes.Equal(pcm, append(slices.Clone(first), last...))) {
			t.Fatalf("bad cached replay (%v / %v)", err, replayErr)
		}
	}
	stall := newAudioStream()
	if _, err := stall.waitChunk(t.Context(), 0, 5*time.Millisecond); err == nil {
		t.Fatal("stalled stream did not time out")
	}
	if id, _, _ := b.audio.add(stall); id != "" {
		t.Fatal("cached timed-out stream")
	}
	ids := make([]string, 17)
	for i := range ids {
		s := newAudioStream()
		s.append(first)
		s.finish(nil)
		ids[i], _, _ = b.audio.add(s)
		if i == 15 {
			res := open(ids[0])
			res.Body.Close()
		}
	}
	for i, want := range []int{200, 404} {
		res := open(ids[i])
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("LRU item %d: %d != %d", i, res.StatusCode, want)
		}
	}
}

func TestStateAndDirectMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lark.db")
	db := testDB(t, path)
	p := peers{deviceStore(db)}
	mint := peer{"lark-mint", "Mint", "127.0.0.2", 8080, time.Now().Unix()}
	if err := p.register(mint); err != nil {
		t.Fatal(err)
	}
	others, err := (peers{deviceStore(db)}).snapshot("cake")
	if err != nil || !slices.Equal(targets(others, "everyone"), []peer{mint}) {
		t.Fatalf("bad peer snapshot %+v %v", others, err)
	}
	for _, cmd := range []string{"Hey Lark, tell MINT: dinner is ready!", "Tell Mint — dinner is ready!", "Send lark-mint, dinner is ready!"} {
		m := directMessage(others, directBody(cmd))
		if m == nil || m.text != "dinner is ready!" || len(targets(others, m.target)) != 1 {
			t.Fatalf("didn't match direct relay: %s", cmd)
		}
	}
	for _, phrase := range []string{"a joke", "a short dinosaur story", "a space fact", "about the Moon", "a riddle", "a quiz"} {
		if directMessage(others, directBody("Send Mint "+phrase)) != nil {
			t.Fatalf("bypassed composition: %s", phrase)
		}
	}
	mint.IP = "127.0.0.4"
	if err := p.register(mint); err != nil {
		t.Fatal(err)
	}
	next, err := (peers{deviceStore(db)}).snapshot("")
	if err != nil || !slices.Equal(next, []peer{mint}) || others[0].IP == mint.IP {
		t.Fatal("register didn't overwrite or modified prior snapshot")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("state mode", err)
	}
}

func TestBridgeAuthAndBody(t *testing.T) {
	b := &bridge{token: "correct", peers: peers{deviceStore(testDB(t, ":memory:"))}}
	for _, tc := range []struct {
		lark, legacy []string
		want         int
	}{
		{[]string{"correct"}, nil, 200},
		{nil, []string{"correct"}, 401},
		{[]string{"wrong"}, nil, 401},
		{[]string{"correct", "correct"}, nil, 401},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/scope", nil)
		for _, token := range tc.lark {
			req.Header.Add("X-Lark-Token", token)
		}
		for _, token := range tc.legacy {
			req.Header.Add("X-EchoEar-Token", token)
		}
		res := httptest.NewRecorder()
		b.ServeHTTP(res, req)
		if res.Code != tc.want {
			t.Fatalf("bridge auth: HTTP %d, want %d", res.Code, tc.want)
		}
	}
	for _, path := range []string{"/api/register", "/api/text", "/api/voice", "/api/intercom", "/api/assets", "/api/history", "/api/reset"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("X-Lark-Token", "correct")
		req.Header.Set("Content-Type", jsonType)
		res := httptest.NewRecorder()
		b.ServeHTTP(res, req)
		if res.Code != 404 {
			t.Fatalf("%s: retired bridge endpoint returned HTTP %d", path, res.Code)
		}
	}
	for _, path := range []string{"/api/devices", "/api/audio/unknown", "/api/tts", "/ota/version.json"} {
		for _, tokens := range [][]string{nil, {"wrong"}, {"correct", "correct"}} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for _, token := range tokens {
				req.Header.Add("X-Lark-Token", token)
			}
			res := httptest.NewRecorder()
			b.ServeHTTP(res, req)
			if res.Code != 401 || res.Body.String() != `{"error":"Unauthorized"}` {
				t.Fatalf("%s: HTTP %d: %s", path, res.Code, res.Body)
			}
		}
	}
	for _, c := range []struct {
		body, kind string
		code       int
	}{
		{`{"text":"Hello"}`, jsonType, 503},
		{`{"text":""}`, jsonType, 400},
		{`[]`, jsonType, 400},
		{`{} trailing`, jsonType, 400},
		{`{}`, pcmType, 415},
		{strings.Repeat(" ", maxJSON+1), jsonType, 413},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/tts", strings.NewReader(c.body))
		req.RemoteAddr = "127.0.0.1:10000"
		req.Header.Set("Content-Type", c.kind)
		req.Header.Set("X-Lark-Token", "correct")
		res := httptest.NewRecorder()
		b.ServeHTTP(res, req)
		if res.Code != c.code {
			t.Fatalf("%s: HTTP %d instead of %d: %s", truncate(c.body, 64), res.Code, c.code, res.Body)
		}
	}
}

func TestOpenCodeStreamingAndFallback(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(strconv.FormatBool(disconnect), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			kick, early := make(chan struct{}), make(chan struct{})
			const first = "Some ocean animals make their own light."
			const reply = first + " They use it to find food.\n```lark-screen\n{\"title\":\"Ocean\"}\n```"
			var waits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/event":
					w.Header().Set("Content-Type", "text/event-stream")
					f := w.(http.Flusher)
					_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
					f.Flush()
					select {
					case <-kick:
					case <-r.Context().Done():
						return
					}
					for _, delta := range []string{"Some ocean animals ", "make their own light. "} {
						fmt.Fprintf(w, "event: message\ndata: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses1\",\"assistantMessageID\":\"msg2\",\"ordinal\":1,\"delta\":%q}}\n\n", delta)
						f.Flush()
					}
					select {
					case <-early:
					case <-r.Context().Done():
						return
					}
					if disconnect {
						return
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses1\"}}\n\n")
					f.Flush()
					<-r.Context().Done()
				case "/api/session/ses1/prompt":
					close(kick)
					_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
				case "/api/session/ses1/message":
					_ = json.NewEncoder(w).Encode(object{"data": []object{
						{"id": "idle", "type": "idle", "outcome": "succeeded"},
						{"id": "msg2", "type": "assistant", "finish": "stop", "content": []object{{"type": "text", "text": reply}}},
						{"id": "msg1", "type": "user"},
					}})
				case "/api/experimental/session/ses1/wait":
					waits.Add(1)
					w.WriteHeader(204)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			ctx, cancelFeed := context.WithCancel(ctx)
			defer cancelFeed()
			path := filepath.Join(t.TempDir(), "service.json")
			if err := writeMap(path, object{"url": srv.URL, "password": "unit"}); err != nil {
				t.Fatal(err)
			}
			o := newOpenCode(ctx, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
				testDB(t, ":memory:"), t.TempDir(), ocSettings{})
			eventually(t, func() bool {
				o.feed.mu.Lock()
				defer o.feed.mu.Unlock()
				return o.feed.connected
			})
			var heard []string
			got, err := o.runTurnStreaming(ctx, "ses1", "test", turnStream{sentence: func(s string) {
				heard = append(heard, s)
				if len(heard) == 1 {
					close(early)
				}
			}}, "", nil)
			if got != reply || err != nil || !slices.Equal(heard, []string{first}) {
				t.Fatalf("didn't overlap or lost final: %q %v %v", got, heard, err)
			}
			if (waits.Load() == 1) != disconnect {
				t.Fatalf("unexpected /wait calls: %d", waits.Load())
			}
		})
	}
}

func TestCredentialReloadOnUnauthorized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-api.json")
	var replacement []byte
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:new")) {
			_, _ = io.WriteString(w, "{}")
			return
		}
		info, _ := os.Stat(path)
		if err := os.WriteFile(path, replacement, 0o600); err == nil {
			_ = os.Chtimes(path, info.ModTime(), info.ModTime())
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	replacement, _ = json.Marshal(object{"url": srv.URL, "password": "new"})
	if err := writeMap(path, object{"url": srv.URL, "password": "old"}); err != nil {
		t.Fatal(err)
	}
	u := upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}}
	if !u.reachable(t.Context(), "/health") || attempts.Load() != 2 {
		t.Fatalf("unauthorized did not reload credentials: %d requests", attempts.Load())
	}
}
