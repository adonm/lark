package main

// Deterministic engines for the cross-process end-to-end test. They replace
// only the neural models: the terminal protocol, capture DSP, RTP, TTS
// streaming and IPC are the real edge implementation.

import (
	"context"
	"math"
	"os"

	"lark/internal/audio"
	"lark/internal/edge"
)

type fakeTranscriber struct{ text string }

func (f *fakeTranscriber) Transcribe(context.Context, []byte) (string, error) { return f.text, nil }
func (f *fakeTranscriber) Close()                                             {}

type fakeSynthesizer struct{}

func (f *fakeSynthesizer) Generate(string, audio.Voice, func() bool) ([]float32, int) {
	raw := make([]float32, audio.SampleRate/4)
	for i := range raw {
		raw[i] = float32(0.18 * math.Sin(2*math.Pi*220*float64(i)/audio.SampleRate))
	}
	return raw, audio.SampleRate
}
func (f *fakeSynthesizer) Close() {}

type fakeTurnDetector struct{}

func (f *fakeTurnDetector) PredictPCM16(pcm []byte) (float32, error) {
	if len(pcm) >= audio.SampleRate {
		return 0.99, nil
	}
	return 0.1, nil
}
func (f *fakeTurnDetector) Close() {}

type fakeVAD struct{}

func (f *fakeVAD) Detect(pcm []byte) bool {
	var sum float64
	samples := len(pcm) / 2
	if samples == 0 {
		return false
	}
	for i := 0; i < samples; i++ {
		v := float64(int16(uint16(pcm[2*i]) | uint16(pcm[2*i+1])<<8))
		sum += v * v
	}
	return math.Sqrt(sum/float64(samples)) > 500
}
func (f *fakeVAD) Close() {}

func fakeEdgeConfig(text string) (edge.Transcriber, edge.Synthesizer, edge.TurnDetector, edge.VADFactory) {
	if text == "" {
		text = "hello there"
	}
	return &fakeTranscriber{text: text}, &fakeSynthesizer{}, &fakeTurnDetector{},
		func() edge.VAD { return &fakeVAD{} }
}

func fakeEdgeText() string {
	return os.Getenv("LARK_EDGE_FAKE_STT")
}
