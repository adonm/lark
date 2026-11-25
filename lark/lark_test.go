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

	"lark/internal/audio"
)

func TestTurnPromptIdentifiesDeviceAndInput(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("AEDT", 11*3600)
	fixed := time.Date(2026, 11, 22, 18, 4, 0, 0, loc)
	for _, tc := range []struct {
		id, source string
	}{
		{"vocat-33d8", "voice"},
		{"box3-b324", "typed"},
	} {
		if got := turnPromptAt(tc.source, "Hi", companionState{companionLook: companionLook{Name: "Cake", Configured: true}}, fixed); got !=
			"[Speaker: someone in person at your device (name unknown) · "+tc.source+"]\nHi\n[state: now: 2026-11-22 18:04 AEDT (+11:00) · listen: off]" {
			t.Fatal(got)
		}
	}
	if got := turnPromptChannelAt("deltachat", "Dinner?", companionState{companionLook: companionLook{Name: "Greg", Configured: true}},
		turnChannel{channel: "deltachat", sender: "Mum", senderKind: "contact"}, fixed); got !=
		"[Speaker: Mum via Delta Chat (contact)]\nDinner?\n[state: now: 2026-11-22 18:04 AEDT (+11:00) · listen: off]" {
		t.Fatal(got)
	}
	if got := turnPromptChannelAt("deltachat", "Hey", companionState{companionLook: companionLook{Name: "Greg", Configured: true}},
		turnChannel{channel: "deltachat", sender: "cake", senderKind: "companion"}, fixed); got !=
		"[Speaker: cake via Delta Chat (companion)]\nHey\n[state: now: 2026-11-22 18:04 AEDT (+11:00) · listen: off]" {
		t.Fatal(got)
	}
}

func TestTurnHeaderOmitsStaticIdentity(t *testing.T) {
	t.Parallel()
	// Identity lives in the session instruction entry, so no header branch may
	// repeat the companion name.
	comp := companionState{companionLook: companionLook{Name: "Cake", Configured: true, Personality: "curious fox"}}
	for _, tc := range []struct {
		source string
		ch     turnChannel
		want   string
	}{
		{"voice", turnChannel{}, "[Speaker: someone in person at your device (name unknown) · voice]"},
		{"typed", turnChannel{reply: &turnReply{name: "Mum"}}, "[Speaker: someone in person at your device (name unknown) · replying to Mum (Delta Chat) · typed]"},
		{"deltachat", turnChannel{channel: "deltachat", sender: "Mum", senderKind: "contact"}, "[Speaker: Mum via Delta Chat (contact)]"},
		{"deltachat", turnChannel{channel: "deltachat", sender: "Mum", senderKind: "contact", private: true}, "[Speaker: Mum via Delta Chat (contact) · private]"},
		{"timer", turnChannel{channel: "timer"}, "[Event: timer]"},
	} {
		if got := turnHeader(tc.source, comp, tc.ch); got != tc.want {
			t.Errorf("turnHeader(%q) = %q, want %q", tc.source, got, tc.want)
		}
	}
	blank := companionState{companionLook: companionLook{Name: "New Companion"}}
	if got := turnHeader("voice", blank, turnChannel{}); got != "[Speaker: someone in person at your device (name unknown) · voice · setup]" {
		t.Errorf("blank companion header = %q", got)
	}
}

func TestStripLarkHeaderDropsIdentityAndState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"[Speaker: someone in person at your device (name unknown) · voice]\nHi\n[state: now: 12:00 · listen: off]", "Hi"},
		{"[Event: timer]\nTimer fired\n[state: now: 12:00]", "Timer fired"},
		{"[You are Cake (cake). Speaker: someone in person at your device (name unknown) · voice]\nHi\n[state: now: 12:00]", "Hi"},
		{"[Speaker: Mum via Delta Chat (contact)]\nHi\n[library: search before facts]\n[state: now: 12:00]", "Hi"},
		{"plain text", "plain text"},
	} {
		if got := stripLarkHeader(tc.in); got != tc.want {
			t.Errorf("stripLarkHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTurnHeaderNeverPutsCompanionInSpeakerPosition(t *testing.T) {
	t.Parallel()
	comp := companionState{companionLook: companionLook{Name: "greg", Configured: true}}
	for _, ch := range []turnChannel{
		{},
		{reply: &turnReply{name: "Mum"}},
		{channel: "deltachat", sender: "Mum", senderKind: "contact"},
		{channel: "deltachat", sender: "Mum", senderKind: "contact", private: true},
	} {
		hdr := turnHeader("voice", comp, ch)
		_, afterSpeaker, ok := strings.Cut(hdr, "Speaker: ")
		if !ok {
			t.Fatalf("header missing Speaker: %q", hdr)
		}
		if strings.Contains(strings.ToLower(afterSpeaker), "greg") {
			t.Fatalf("companion name appeared in speaker position: %q", hdr)
		}
	}
}

func TestLocalTimeAcrossAgentSurfaces(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("AEDT", 11*3600)
	fixed := time.Date(2026, 11, 22, 18, 4, 0, 0, loc)
	if got := formatLocalTimeIn(fixed, loc); got != "2026-11-22 18:04 AEDT (+11:00)" {
		t.Fatalf("formatLocalTimeIn = %q", got)
	}

	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.loc = loc
		b.history = &chatHistory{db: b.db}
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	s := toolTest(t, base, "state", object{"include": []string{}}, false)
	nowObj := s["now"].(object)
	localStr := str(nowObj["local"])
	if !strings.Contains(localStr, "AEDT (+11:00)") {
		t.Fatalf("state now.local = %q, want AEDT (+11:00)", localStr)
	}
	if !strings.HasSuffix(str(nowObj["rfc3339"]), "+11:00") {
		t.Fatalf("state now.rfc3339 = %q, want +11:00", str(nowObj["rfc3339"]))
	}

	id := "greg"
	if _, err := b.createCompanion(t.Context(), object{"id": id, "name": "Greg", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	// Timer `at` without offset must be interpreted in local time.
	atLocal := time.Now().In(loc).Add(30 * time.Minute).Format("2006-01-02T15:04:05")
	timer := toolTest(t, base, "patch", object{"companion_id": id, "timer": object{"op": "set", "at": atLocal, "label": "Tea"}}, false)["timer"].(object)
	if !strings.Contains(str(timer["fire_at_local"]), "AEDT (+11:00)") {
		t.Fatalf("timer fire_at_local = %q, want AEDT (+11:00)", str(timer["fire_at_local"]))
	}
	if !strings.HasSuffix(str(timer["fire_at"]), "+11:00") {
		t.Fatalf("timer fire_at = %q, want +11:00", str(timer["fire_at"]))
	}
}

func TestSentencesAndFence(t *testing.T) {
	t.Parallel()
	want := []string{"Whoa!", "Dr. Lee found a 3.2-inch shell.", "Ooh, awesome question! What made it spiral?"}
	if got := audio.SplitSentences("**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral?"); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	var ex audio.SentenceExtractor
	var clauses []string
	for _, chunk := range []string{"Well, ", "short, ", "and now a much longer opening clause, ", "followed by more, ", "and the end."} {
		clauses = append(clauses, ex.Add(chunk)...)
	}
	clauses = append(clauses, ex.Flush()...)
	wantStream := []string{"Well, short, and now a much longer opening clause,", "followed by more, and the end."}
	if !slices.Equal(clauses, wantStream) {
		t.Fatalf("audio.SentenceExtractor clause flush = %q, want %q", clauses, wantStream)
	}
	var s audio.SentenceExtractor
	var streamed []string
	for _, delta := range []string{"**Whoa!** Dr. Lee found a 3.", "2-inch shell. ", "Ooh, awesome question!", " What made it spiral in circles?\n", "``", "`lark-screen", "\nNever say this. \n```", "Not eager even after fence. "} {
		streamed = append(streamed, s.Add(delta)...)
	}
	want[2] = "Ooh, awesome question! What made it spiral in circles?"
	if !s.Halted || !slices.Equal(streamed, want) {
		t.Fatal(streamed)
	}
}

func TestSentenceExtractorFragmentation(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral? ",
		"\t A. B. Wood and Prof. Façade saw it…\u2002Then it returned.\u00a0“Really?” No way. ",
		"What happened at St. James? “Time to see the world!” she said. “Wait.”...? unfinished",
		"`inline` and ``more`` markup. Yes.```svg\n This is never read. ```",
		"NotMr. NotProf. e.g. a note. A.B. This can finish. _",
	} {
		raw, _, fence := strings.Cut(text, "```")
		unmarked := strings.NewReplacer("*", "", "_", "", "`", "", "#", "").Replace(raw)
		want := audio.Sentences(audio.PlainText(raw), false, fence || unmarked != strings.TrimRightFunc(unmarked, unicode.IsSpace))
		for size := 1; size <= len(text); size++ {
			var extractor audio.SentenceExtractor
			var got []string
			for i := 0; i < len(text); i += size {
				got = append(got, extractor.Add(text[i:min(i+size, len(text))])...)
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
				var extractor audio.SentenceExtractor
				for i := 0; i < len(text); i += size {
					extractor.Add(text[i:min(i+size, len(text))])
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
	t.Parallel()
	rms := func(pcm []byte) float64 {
		var sum float64
		for i := 0; i < len(pcm); i += 2 {
			v := float64(int16(le.Uint16(pcm[i:])))
			sum += v * v
		}
		return math.Sqrt(sum / float64(len(pcm)/2))
	}
	pcm, err := audio.SynthPCM(sineFloats(24000, 1000, .25), 24000)
	if err != nil || len(pcm) != 32000 || math.Abs(rms(pcm)-10426) > 100 {
		t.Fatalf("resampler passband: %d bytes, error %v", len(pcm), err)
	}
	alias, err := audio.SynthPCM(sineFloats(24000, 10000, .25), 24000)
	if err != nil || rms(alias[64:len(alias)-64]) > 110 {
		t.Fatalf("resampler aliasing: %v", err)
	}
	loud := bytes.Repeat([]byte{0x80, 0x3e}, 60)
	quiet := bytes.Repeat([]byte{64, 0}, 120)
	normalized := int16(le.Uint16(audio.NormalizeVoicePCM(loud)))
	if normalized < 25000 || normalized > 26100 || !bytes.Equal(audio.NormalizeVoicePCM(quiet), quiet) {
		t.Fatal("bad normalization or noise-floor guard")
	}

}

func BenchmarkSincOneSecond(b *testing.B) {
	raw := sineFloats(24000, 1000, .25)
	for b.Loop() {
		_, _ = audio.SynthPCM(raw, 24000)
	}
}

func TestStateAndDirectMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "lark.db")
	db := testDB(t, path)
	b := &larkServer{db: db}
	if _, err := b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint", "form": "a cat"}); err != nil {
		t.Fatal(err)
	}
	mint := companionTarget{ID: "mint", Name: "Mint"}
	others, err := b.companionTargets("cake")
	if err != nil || !slices.Equal(targets(others, "everyone"), []companionTarget{mint}) {
		t.Fatalf("bad companion targets %+v %v", others, err)
	}
	for _, cmd := range []string{"Hey Lark, tell MINT: dinner is ready!", "Tell Mint — dinner is ready!", "Send mint, dinner is ready!"} {
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
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("state mode", err)
	}
}

func TestBridgeAuthAndBody(t *testing.T) {
	t.Parallel()
	b := &larkServer{token: "correct", db: testDB(t, ":memory:")}
	for _, tc := range []struct {
		lark []string
		want int
	}{
		{[]string{"correct"}, 200},
		{nil, 401},
		{[]string{"wrong"}, 401},
		{[]string{"correct", "correct"}, 401},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/companions", nil)
		for _, token := range tc.lark {
			req.Header.Add("X-Lark-Token", token)
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
	for _, path := range []string{"/api/companions", "/api/terminals", "/ota/version.json"} {
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
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "192.168.1.50:12345"
	req.Header.Set("X-Lark-Token", "correct")
	res := httptest.NewRecorder()
	b.ServeHTTP(res, req)
	if res.Code != 404 {
		t.Fatalf("/debug/pprof/ from non-loopback returned HTTP %d", res.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	res = httptest.NewRecorder()
	b.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatalf("/debug/pprof/ without token returned HTTP %d", res.Code)
	}
	req.Header.Set("X-Lark-Token", "correct")
	res = httptest.NewRecorder()
	b.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("/debug/pprof/ on loopback with token returned HTTP %d", res.Code)
	}
}

func TestOpenCodeStreamingAndFallback(t *testing.T) {
	t.Parallel()
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
			o.feed.sig.wait(t, func() bool {
				o.feed.mu.Lock()
				defer o.feed.mu.Unlock()
				return o.feed.connected
			})
			var heard []string
			got, err := o.runTurnStreamingWithModel(ctx, "ses1", "test", ocModel{}, turnStream{sentence: func(s string) {
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
	t.Parallel()
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

func TestOpenCodeExplicitEndpointAndDiscovery(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	password := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok && user != "opencode" {
			t.Error("unexpected Basic auth user")
		}
		password <- pass
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	dir := filepath.Join(state, "opencode")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMap(filepath.Join(dir, "service.json"), object{"url": server.URL, "password": "discovered"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ url, password, want string }{
		{"", "ignored", "discovered"},
		{server.URL, "dedicated", "dedicated"},
		{server.URL, "", ""},
	} {
		api, err := openCodeUpstream(server.Client(), state, tc.url, tc.password)
		if err != nil || !api.reachable(t.Context(), "/api/info") {
			t.Fatalf("OpenCode endpoint unavailable: %v", err)
		}
		if <-password != tc.want {
			t.Fatal("OpenCode sent the wrong Authorization header")
		}
	}
	if _, err := openCodeUpstream(server.Client(), state, "http://example.org:49375", "secret"); err == nil {
		t.Fatal("accepted a nonlocal OpenCode endpoint")
	}
}

func newCompanion(t *testing.T, b *larkServer, id string) companionState {
	t.Helper()
	c, err := b.createCompanion(t.Context(), object{"id": id, "name": strings.ToUpper(id[:1]) + id[1:], "form": "a cat"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
