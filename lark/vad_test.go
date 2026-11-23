package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSileroVADFeedsWholeWindows(t *testing.T) {
	t.Parallel()
	s := &sileroVAD{}
	pcm := make([]byte, endpointBytes) // 320 samples per capture frame
	var sizes []int
	answers := []bool{false, false, true, false}
	calls := 0
	out := func(w []float32) bool {
		sizes = append(sizes, len(w))
		v := answers[calls%len(answers)]
		calls++
		return v
	}
	for range 5 { // 1600 samples -> windows at 512, 1024, 1536
		s.feed(pcm, out)
	}
	want := []int{sileroWindow, sileroWindow, sileroWindow}
	if len(sizes) != len(want) {
		t.Fatalf("accept called %d times (%v), want %d", len(sizes), sizes, len(want))
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("window %d = %d samples, want %d", i, sizes[i], want[i])
		}
	}
	if !s.voiced {
		t.Fatal("last decision was not kept for the partial window")
	}
	if s.n != 1600-3*sileroWindow {
		t.Fatalf("leftover = %d samples, want %d", s.n, 1600-3*sileroWindow)
	}
}

func vadModelPath() string {
	if p := os.Getenv("LARK_VAD_MODEL"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark", "stt-models", sileroModel)
}

// Loading per capture measured ~23 ms; reuse after the first capture is a
// reset. Both benchmarks skip without the pinned model.
func BenchmarkSileroVADLoad(b *testing.B) {
	path := vadModelPath()
	if path == "" {
		b.Skip("no home directory")
	}
	if _, err := os.Stat(path); err != nil {
		b.Skip("no VAD model")
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := loadSileroVAD(path)
		if s == nil {
			b.Fatal("cannot load VAD")
		}
		s.destroy()
	}
}

func BenchmarkSileroVADAcquire(b *testing.B) {
	path := vadModelPath()
	if path == "" {
		b.Skip("no home directory")
	}
	if _, err := os.Stat(path); err != nil {
		b.Skip("no VAD model")
	}
	if s := newSileroVAD(path); s != nil {
		s.close()
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := newSileroVAD(path)
		if s == nil {
			b.Fatal("cannot acquire VAD")
		}
		s.close()
	}
}
