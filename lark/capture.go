package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// debugCaptureDir is local to the host, mode 0700, and never synced or sent
// anywhere. Captures exist only for the user's own microphone audit.
func debugCaptureDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark", "debug")
}

func writeCaptureWAV(path string, pcm []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	hdr := wavHeader(len(pcm))
	out := make([]byte, len(hdr)+len(pcm))
	copy(out, hdr[:])
	copy(out[len(hdr):], pcm)
	return os.WriteFile(path, out, 0o600)
}

func (b *larkServer) captureAPI(r *http.Request) (any, error) {
	obj, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	companionID := str(obj["companion_id"])
	if companionID == "" {
		return nil, problem(400, "companion_id is required")
	}
	seconds := 20
	if raw, ok := obj["seconds"]; ok {
		n, ok := companionInt(raw)
		if !ok || n < 1 || n > 60 {
			return nil, problem(400, "seconds must be between 1 and 60")
		}
		seconds = n
	}
	if _, ok := b.companionByID(companionID); !ok {
		return nil, problem(404, "Companion not found")
	}
	if b.hub == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	span := time.Duration(seconds) * time.Second
	b.hub.mu.Lock()
	d := b.hub.voiceOwner(companionID)
	var c *endpointCapture
	if d != nil && b.hub.startCaptureWindow(d, time.Now(), span) {
		c = d.recording.endpoint
	}
	b.hub.mu.Unlock()
	if c == nil {
		return nil, problem(409, "No listening device available for this companion")
	}
	if debugEndpoint {
		log.Printf("capture api term=%s span=%v ctxErr=%v", d.id, span, d.client.ctx.Err())
	}
	var result captureResult
	select {
	case result = <-c.result:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	if debugEndpoint {
		log.Printf("capture api result term=%s bytes=%d err=%v ctxErr=%v", d.id, len(result.pcm), result.err, d.client.ctx.Err())
	}
	if len(result.pcm) == 0 {
		return nil, problem(409, "No microphone audio arrived")
	}
	name := fmt.Sprintf("capture-%s-%s.wav", companionID, time.Now().Format("20060102-150405"))
	path := filepath.Join(debugCaptureDir(), name)
	if err := writeCaptureWAV(path, result.pcm); err != nil {
		return nil, problem(500, "Cannot write capture: "+err.Error())
	}
	return object{
		"path": path, "companion_id": companionID, "seconds": seconds,
		"audio_ms": len(result.pcm) * 1000 / (2 * sampleRate), "bytes": len(result.pcm),
	}, nil
}

// sttAPI is the local-only transcription probe behind `lark stt-bench`. It
// accepts a WAV (16 kHz mono) or raw s16le PCM.
func (b *larkServer) sttAPI(r *http.Request) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, problem(400, "Cannot read audio")
	}
	pcm := raw
	if len(raw) >= 44 && bytes.Equal(raw[:4], []byte("RIFF")) {
		pcm, err = parseWAVPCM16k(raw)
		if err != nil {
			return nil, problem(400, err.Error())
		}
	}
	if len(pcm) < 2 || len(pcm)%2 != 0 {
		return nil, problem(400, "Expected 16 kHz mono s16le PCM or WAV")
	}
	if b.sttFunc() == nil {
		return nil, problem(503, "STT unavailable")
	}
	start := time.Now()
	text, err := b.decodeClip(r.Context(), pcm)
	elapsed := time.Since(start)
	if err != nil {
		return nil, problem(502, err.Error())
	}
	return object{
		"transcript": text,
		"stt_ms":     milliseconds(elapsed),
		"audio_ms":   len(pcm) * 1000 / (2 * sampleRate),
		"engine":     sttEngine,
	}, nil
}

func captureUsage() error {
	return errors.New("usage: lark capture <companion> [seconds]")
}
