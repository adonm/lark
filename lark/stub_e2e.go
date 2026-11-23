//go:build e2e

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/chatmail/rpc-client-go/v2/deltachat"
)

// The e2e build replaces the three model-backed pieces of the bridge with
// scripts so the QA matrix can run on any machine, in seconds:
//
//   - STT matches mic audio to synthetic fixture clips by energy envelope and
//     returns that clip's transcript.
//   - TTS returns a short tone without running a model.
//   - Delta Chat uses an in-memory RPC stub driven by /api/e2e/stub/dc.
//
// Endpointing still runs the production policy. It uses an RMS VAD instead of
// Silero and has no Smart Turn model, so those two decisions are weaker here
// than on a real board. docs/testing.md lists the difference.

type stubState struct {
	mu       sync.Mutex
	fixtures []stubFixture
	dc       *stubDCRPC

	dcContact, dcChat, dcMsg uint32
}

var stub = &stubState{dc: newStubDCRPC(), dcContact: 100, dcChat: 100, dcMsg: 100}

func stubSetup(string) *stubConfig {
	if os.Getenv("LARK_STUB") != "1" {
		return nil
	}
	if dir := os.Getenv("LARK_STUB_FIXTURES"); dir != "" {
		if err := stub.loadFixtures(dir); err != nil {
			log.Printf("stub voice fixtures unavailable: %v", err)
		}
	}
	return &stubConfig{
		stt:        stub.transcribe,
		openEngine: func() (speechEngine, error) { return stubEngine{}, nil },
		endpoint: &endpointSettings{
			newVAD: func() endpointVAD { return &stubVAD{threshold: stubVADThreshold} },
		},
		startDC: stub.startDC,
	}
}

// A fixture is a synthetic WAV plus the transcript it should produce. The
// e2e bridge matches mic audio to these clips by energy envelope, so scripted
// STT still depends on the audio that actually arrived instead of call order.
type stubFixture struct {
	File       string `json:"file"`
	Transcript string `json:"transcript"`
	env        []float64
	speechEnd  int
}

func (s *stubState) loadFixtures(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var m struct {
		Fixtures []stubFixture `json:"fixtures"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for i := range m.Fixtures {
		pcm, err := readWAVPCM16k(filepath.Join(dir, m.Fixtures[i].File))
		if err != nil {
			return fmt.Errorf("%s: %w", m.Fixtures[i].File, err)
		}
		m.Fixtures[i].env = envelope(pcm)
		for j, rms := range m.Fixtures[i].env {
			if rms >= stubVADThreshold {
				m.Fixtures[i].speechEnd = j
			}
		}
	}
	s.mu.Lock()
	s.fixtures = m.Fixtures
	s.mu.Unlock()
	return nil
}

const stubVADThreshold = 220.0

func envelope(pcm []byte) []float64 {
	frames := make([]float64, 0, len(pcm)/endpointBytes+1)
	for i := 0; i+endpointBytes <= len(pcm); i += endpointBytes {
		frames = append(frames, frameRMS(pcm[i:i+endpointBytes]))
	}
	return frames
}

func normalized(frames []float64) ([]float64, float64) {
	if len(frames) == 0 {
		return nil, 0
	}
	mean := 0.0
	for _, v := range frames {
		mean += v
	}
	mean /= float64(len(frames))
	out := make([]float64, len(frames))
	norm := 0.0
	for i, v := range frames {
		out[i] = v - mean
		norm += out[i] * out[i]
	}
	return out, math.Sqrt(norm)
}

// bestWindow returns the fixture offset whose envelope correlates best with
// the clip, plus the normalized score.
func bestWindow(fix, clip []float64) (int, float64) {
	n := len(clip)
	if n == 0 || len(fix) < n {
		return 0, 0
	}
	c, cnorm := normalized(clip)
	if cnorm == 0 {
		return 0, 0
	}
	bestOffset, best := 0, 0.0
	for off := 0; off+n <= len(fix); off++ {
		w, wnorm := normalized(fix[off : off+n])
		if wnorm == 0 {
			continue
		}
		dot := 0.0
		for i := range c {
			dot += c[i] * w[i]
		}
		if score := dot / (cnorm * wnorm); score > best {
			bestOffset, best = off, score
		}
	}
	return bestOffset, best
}

func (s *stubState) transcribe(_ context.Context, pcm []byte) (string, error) {
	s.mu.Lock()
	fixtures := s.fixtures
	s.mu.Unlock()
	if len(pcm) == 0 || len(fixtures) == 0 {
		return "", nil
	}
	clip := envelope(pcm)
	if len(clip) < 8 {
		return "", nil
	}
	lastVoiced := -1
	for i, rms := range clip {
		if rms >= stubVADThreshold {
			lastVoiced = i
		}
	}
	if lastVoiced < 0 {
		return "", nil
	}
	best := -1
	bestScore := 0.0
	bestOffset := 0
	for i := range fixtures {
		off, score := bestWindow(fixtures[i].env, clip)
		if score > bestScore {
			best, bestScore, bestOffset = i, score, off
		}
	}
	if best < 0 || bestScore < 0.80 {
		return "", nil
	}
	f := fixtures[best]
	// Truncate at the coverage the clip actually heard. Trailing probe
	// silence must not advance the transcript past what was voiced.
	denom := max(1, f.speechEnd+1)
	frac := math.Min(1, float64(bestOffset+lastVoiced+1)/float64(denom))
	words := strings.Fields(f.Transcript)
	n := int(math.Round(frac * float64(len(words))))
	if n >= len(words) {
		return f.Transcript, nil
	}
	return strings.Join(words[:n], " "), nil
}

type stubEngine struct{}

// generate returns 900 ms of a soft tone. Long enough that the Stop gesture
// can land mid-playback, and instant to compute.
func (stubEngine) generate(string, voiceChoice, func() bool) ([]float32, int) {
	const n = sampleRate * 900 / 1000
	out := make([]float32, n)
	for i := range out {
		out[i] = 0.08 * float32(math.Sin(2*math.Pi*440*float64(i)/sampleRate))
	}
	return out, sampleRate
}

func (stubEngine) close() {}

type stubVAD struct{ threshold float64 }

func (v *stubVAD) detect(pcm []byte) bool { return frameRMS(pcm) >= v.threshold }
func (v *stubVAD) close()                 {}

func (s *stubState) startDC(b *larkServer) {
	b.dc.mu.Lock()
	b.dc.rpc = s.dc
	b.dc.mu.Unlock()
	b.dc.ready.Store(true)
	go func() {
		defer close(b.dc.doneCh)
		if err := b.dc.syncAccounts(b.root); err != nil && b.root.Err() == nil {
			fmt.Fprintf(os.Stderr, "stub Delta Chat sync: %v\n", err)
		}
		select {
		case <-b.dc.stopCh:
		case <-b.root.Done():
		}
		b.dc.ready.Store(false)
	}()
}

func (s *stubState) injectDC(ctx context.Context, b *larkServer, companionID, name, address, text string) error {
	c, ok := b.companionByID(companionID)
	if !ok {
		return problem(404, "Unknown companion")
	}
	accID, err := b.dc.ensureCompanion(ctx, c)
	if err != nil {
		return err
	}
	if address == "" {
		address = strings.ToLower(name) + "@test.example"
	}
	s.mu.Lock()
	s.dcContact++
	contactID := s.dcContact
	s.dcChat++
	chatID := s.dcChat
	s.dcMsg++
	msgID := s.dcMsg
	s.dc.mu.Lock()
	s.dc.contacts[accID][contactID] = deltachat.Contact{Id: contactID, DisplayName: name, Name: name,
		Address: address, IsVerified: true}
	s.dc.chats[accID][chatID] = deltachat.BasicChat{Id: chatID, ChatType: deltachat.ChatTypeSingle}
	s.dc.chatContacts[accID][chatID] = []uint32{deltachat.ContactSelf, contactID}
	s.dc.msgs[accID][msgID] = deltachat.Message{Id: msgID, ChatId: chatID, FromId: contactID,
		Text: text, ViewType: deltachat.ViewtypeText}
	s.dc.mu.Unlock()
	s.mu.Unlock()
	b.dc.handleMessage(ctx, accID, msgID)
	return nil
}

func (s *stubState) sent() []object {
	s.dc.mu.Lock()
	defer s.dc.mu.Unlock()
	out := make([]object, 0, len(s.dc.sent))
	for _, m := range s.dc.sent {
		row := object{"account_id": m.accID, "chat_id": m.chatID}
		if m.data.Text != nil {
			row["text"] = *m.data.Text
		}
		if m.data.File != nil {
			row["file"] = *m.data.File
		}
		out = append(out, row)
	}
	return out
}

func stubHTTP(b *larkServer, r *http.Request, path string) (any, error, bool) {
	if !strings.HasPrefix(path, "/api/e2e/stub/") {
		return nil, nil, false
	}
	switch {
	case path == "/api/e2e/stub/dc/message" && r.Method == http.MethodPost:
		body, err := jsonBody(r)
		if err != nil {
			return nil, err, true
		}
		err = stub.injectDC(r.Context(), b, str(body["companion_id"]), str(body["name"]),
			str(body["address"]), str(body["text"]))
		if err != nil {
			return nil, err, true
		}
		return object{"ok": true}, nil, true
	case path == "/api/e2e/stub/dc/sent" && r.Method == http.MethodGet:
		return object{"messages": stub.sent()}, nil, true
	case path == "/api/e2e/stub/dc/accounts" && r.Method == http.MethodGet:
		// Provision accounts on demand so a test can pair companions that were
		// created after the service's startup sync.
		if comps, err := b.allCompanions(r.Context()); err == nil {
			for _, c := range comps {
				_, _ = b.dc.ensureCompanion(r.Context(), c)
			}
		}
		b.dc.mu.Lock()
		addrs := make(map[string]string, len(b.dc.addrs))
		for id, addr := range b.dc.addrs {
			addrs[id] = addr
		}
		b.dc.mu.Unlock()
		return object{"accounts": addrs}, nil, true
	case path == "/api/e2e/stub/state" && r.Method == http.MethodGet:
		stub.mu.Lock()
		count := len(stub.fixtures)
		stub.mu.Unlock()
		return object{"stub": true, "fixtures": count}, nil, true
	}
	return nil, problem(404, "Not found"), true
}
