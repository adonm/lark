package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"lark/internal/audio"
)

// Transcriber is the persistent speech-to-text backend. The real one is a
// Vulkan Parakeet worker; tests inject a deterministic fake.
type Transcriber interface {
	Transcribe(context.Context, []byte) (string, error)
	Close()
}

// Synthesizer is the persistent TTS backend. Generate must be safe to call
// from one stream goroutine at a time; the edge serializes per stream.
type Synthesizer interface {
	Generate(text string, voice audio.Voice, keepGoing func() bool) ([]float32, int)
	Close()
}

// TurnDetector predicts whether an utterance is semantically complete.
type TurnDetector interface {
	PredictPCM16([]byte) (float32, error)
	Close()
}

// VAD is one capture's voice-activity detector, reset between turns.
type VAD interface {
	Detect([]byte) bool
	Close()
}

// VADFactory builds a pooled detector for a new capture.
type VADFactory func() VAD

var modelNames = []string{"parakeet", "kokoro", "smart_turn", "silero"}

const (
	sileroModel  = "silero_vad_v5.onnx"
	sileroSHA256 = "6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396"
)

// Models holds the loaded speech backends and their public state. Loading is
// concurrent; the ready event carries a consistent snapshot.
type Models struct {
	mu    sync.RWMutex
	stt   Transcriber
	tts   Synthesizer
	turn  TurnDetector
	vad   VADFactory
	state map[string]string
	errs  map[string]string
}

func newModels() *Models {
	m := &Models{state: make(map[string]string), errs: make(map[string]string)}
	for _, name := range modelNames {
		m.state[name] = "loading"
	}
	return m
}

func (m *Models) install(cfg Config) error {
	for _, name := range modelNames {
		m.state[name] = "loading"
	}
	if cfg.Transcriber != nil {
		m.stt, m.state["parakeet"] = cfg.Transcriber, "ready"
	}
	if cfg.Synthesizer != nil {
		m.tts, m.state["kokoro"] = cfg.Synthesizer, "ready"
	}
	if cfg.TurnDetector != nil {
		m.turn, m.state["smart_turn"] = cfg.TurnDetector, "ready"
	}
	if cfg.VADFactory != nil {
		m.vad, m.state["silero"] = cfg.VADFactory, "ready"
	}
	if cfg.DisableModels {
		for _, name := range modelNames {
			if m.state[name] == "loading" {
				m.state[name] = "disabled"
			}
		}
	}
	return nil
}

func (m *Models) setReady(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[name] = "ready"
	delete(m.errs, name)
}

func (m *Models) setError(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[name] = "error"
	if err != nil {
		m.errs[name] = err.Error()
	}
}

func (m *Models) snapshot() map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]any, len(m.state))
	for _, name := range modelNames {
		entry := map[string]any{"state": m.state[name]}
		if e := m.errs[name]; e != "" {
			entry["error"] = e
		}
		out[name] = entry
	}
	return out
}

func (m *Models) stateOf(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state[name]
}

func (m *Models) states() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]string, len(m.state))
	for k, v := range m.state {
		out[k] = v
	}
	return out
}

func (m *Models) transcriber() Transcriber {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stt
}

func (m *Models) synthesizer() Synthesizer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tts
}

func (m *Models) turnDetector() TurnDetector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.turn
}

func (m *Models) newVAD() VAD {
	m.mu.RLock()
	f := m.vad
	m.mu.RUnlock()
	if f == nil {
		return nil
	}
	return f()
}

func (m *Models) load(name, dataDir string) error {
	switch name {
	case "parakeet":
		path := filepath.Join(dataDir, "stt-models", audio.ParakeetModel)
		if err := audio.CheckParakeetModel(path); err != nil {
			return err
		}
		stt, err := audio.OpenParakeet(path)
		if err != nil {
			return err
		}
		// One real decode validates the Vulkan backend before readiness.
		if _, err := stt.Transcribe(context.Background(), make([]byte, audio.SampleRate*2)); err != nil {
			stt.Close()
			return err
		}
		m.mu.Lock()
		if m.stt == nil {
			m.stt = stt
		}
		m.mu.Unlock()
	case "kokoro":
		tts, err := audio.OpenKokoro(audio.TTSModelDir(dataDir), min(audio.KokoroThreads, runtime.GOMAXPROCS(0)))
		if err != nil {
			return err
		}
		m.mu.Lock()
		if m.tts == nil {
			m.tts = tts
		}
		m.mu.Unlock()
	case "smart_turn":
		path := filepath.Join(dataDir, "stt-models", audio.SmartTurnModel)
		engine, err := audio.Open(path, min(4, runtime.GOMAXPROCS(0)))
		if err != nil {
			return err
		}
		m.mu.Lock()
		if m.turn == nil {
			m.turn = engine
		}
		m.mu.Unlock()
	case "silero":
		path := filepath.Join(dataDir, "stt-models", sileroModel)
		if err := checkSileroModel(path); err != nil {
			return err
		}
		probe := newSileroVAD(path)
		if probe == nil {
			return fmt.Errorf("could not load Silero v5 model from %s", path)
		}
		probe.Close()
		m.mu.Lock()
		if m.vad == nil {
			m.vad = func() VAD { return newSileroVAD(path) }
		}
		m.mu.Unlock()
	default:
		return fmt.Errorf("unknown model %q", name)
	}
	return nil
}

func (m *Models) close() {
	m.mu.Lock()
	stt, tts, turn := m.stt, m.tts, m.turn
	m.stt, m.tts, m.turn = nil, nil, nil
	m.mu.Unlock()
	if stt != nil {
		stt.Close()
	}
	if tts != nil {
		tts.Close()
	}
	if turn != nil {
		turn.Close()
	}
}

func checkSileroModel(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read Silero v5 model; run scripts/setup-host.sh")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != sileroSHA256 {
		return errors.New("Silero model does not match the pinned Silero v5 ONNX")
	}
	return nil
}
