package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"lark/internal/audio"
)

const sampleRate = audio.SampleRate
const parakeetModel = audio.ParakeetModel
const sttEngine = "parakeet"
const smartTurnModel = audio.SmartTurnModel

var le = binary.LittleEndian

// setSTT swaps the transcription backend. Tests change it mid-flight while
// capture probes read it.
func (b *larkServer) setSTT(fn func(context.Context, []byte) (string, error)) {
	b.sttMu.Lock()
	b.stt = fn
	b.sttMu.Unlock()
}

func (b *larkServer) sttFunc() func(context.Context, []byte) (string, error) {
	b.sttMu.RLock()
	defer b.sttMu.RUnlock()
	return b.stt
}

func (b *larkServer) decodeClip(ctx context.Context, pcm []byte) (string, error) {
	fn := b.sttFunc()
	if fn != nil {
		return fn(ctx, pcm)
	}
	// Parakeet loads in the background after the listener is up. Wait for it
	// (bounded) so the first turn after a restart is not an STT failure.
	if b.models.stt != nil {
		waitCtx, cancel := context.WithTimeout(ctx, modelWait)
		defer cancel()
		if err := b.models.stt.await(waitCtx); err != nil {
			return "", fmt.Errorf("STT unavailable: %w", err)
		}
		if fn = b.sttFunc(); fn != nil {
			return fn(ctx, pcm)
		}
	}
	return "", errors.New("STT unavailable")
}

// The host can check the installed TTS and STT together without creating a
// device, touching chat history, or routing audio to a board.
func (b *larkServer) probeSTT(r *http.Request) (object, error) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !net.ParseIP(host).IsLoopback() {
		return nil, problem(404, "Not found")
	}
	if b.sttFunc() == nil || b.worker == nil || !b.worker.ready.Load() {
		return nil, problem(503, "Speech engines not ready")
	}
	ctx, cancel := context.WithTimeout(r.Context(), ttsWait)
	defer cancel()
	pcm, err := b.worker.synthesize(ctx, heartVoice, "The moon reflects sunlight.")
	if err != nil {
		return nil, problem(502, "STT probe synthesis failed")
	}
	start := time.Now()
	text, err := b.decodeClip(ctx, pcm)
	if err != nil {
		return nil, problem(502, "STT probe transcription failed")
	}
	return object{"transcript": text, "stt_ms": time.Since(start).Milliseconds(), "clip_ms": len(pcm) * 1000 / (2 * sampleRate)}, nil
}
