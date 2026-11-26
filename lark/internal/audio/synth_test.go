package audio

import (
	"context"
	"math"
	"testing"
	"time"
)

func collect(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	var pcm []byte
	for chunk := range ch {
		pcm = append(pcm, chunk...)
	}
	return pcm
}

func peak(pcm []byte) int {
	best := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		if v < 0 {
			v = -v
		}
		best = max(best, v)
	}
	return best
}

func TestMetronomeExactBeatSpacing(t *testing.T) {
	spec, err := NewMetronomeSpec(120, 4, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.PeriodSamples(); got != 8000 {
		t.Fatalf("period = %d, want 8000", got)
	}
	ch, err := MetronomeStream(context.Background(), spec, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pcm := collect(t, ch)
	if want := 5 * SampleRate * 2; len(pcm) != want {
		t.Fatalf("length = %d, want %d", len(pcm), want)
	}
	period := spec.PeriodSamples()
	accent, light := 0, 0
	for beat := 0; beat < 10; beat++ {
		start := beat * period * 2
		end := min(len(pcm), start+SampleRate*60/1000*2)
		p := peak(pcm[start:end])
		if p == 0 {
			t.Fatalf("beat %d is silent", beat)
		}
		if beat%spec.Beats == 0 {
			accent = max(accent, p)
		} else {
			light = max(light, p)
		}
		// The gap between the 48 ms click and the next beat must stay silent.
		gapStart := min(len(pcm), start+SampleRate*70/1000*2)
		gapEnd := min(len(pcm), start+period*2)
		if p := peak(pcm[gapStart:gapEnd]); p != 0 {
			t.Fatalf("beat %d gap peak = %d", beat, p)
		}
	}
	if accent <= light {
		t.Fatalf("accent peak %d is not louder than light %d", accent, light)
	}
}

func TestMetronomeAlternateTempoIsDriftFree(t *testing.T) {
	// 137 BPM rounds to 7007 samples per beat; ten beats must still hit the
	// exact integer lattice, not a floating accumulator.
	spec, err := NewMetronomeSpec(137, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	period := spec.PeriodSamples()
	if want := int(math.Round(16000.0 * 60.0 / 137.0)); period != want {
		t.Fatalf("period = %d, want %d", period, want)
	}
	ch, err := MetronomeStream(context.Background(), spec, 1000)
	if err != nil {
		t.Fatal(err)
	}
	pcm := collect(t, ch)
	for beat := 0; beat*period < len(pcm)/2; beat++ {
		start := beat * period * 2
		if start+200 > len(pcm) {
			break
		}
		if peak(pcm[start:start+200]) == 0 {
			t.Fatalf("beat %d missing at sample %d", beat, beat*period)
		}
	}
}

func TestMetronomeStreamCancels(t *testing.T) {
	spec, err := NewMetronomeSpec(60, 4, 300)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := MetronomeStream(ctx, spec, 512)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metronome stream did not stop")
	}
}

func TestParseNote(t *testing.T) {
	cases := []struct {
		name string
		hz   float64
		rest bool
	}{
		{"C4", 261.625565, false},
		{"F#4", 369.994423, false},
		{"Bb3", 233.081881, false},
		{"A4", 440, false},
		{"C5", 523.251131, false},
		{"R", 0, true},
		{"rest", 0, true},
	}
	for _, tc := range cases {
		hz, err := ParseNote(tc.name)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.rest {
			if hz != 0 {
				t.Fatalf("%s = %f, want rest", tc.name, hz)
			}
			continue
		}
		if math.Abs(hz-tc.hz) > 0.01 {
			t.Fatalf("%s = %f, want %f", tc.name, hz, tc.hz)
		}
	}
	for _, bad := range []string{"", "H4", "C9", "C", "C#", "C##4", "X4"} {
		if _, err := ParseNote(bad); err == nil {
			t.Fatalf("%q parsed, want error", bad)
		}
	}
}

func TestSynthesizeTuneStyles(t *testing.T) {
	notes := []TuneNote{{Note: "C4", Beats: 0.5}, {Note: "R", Beats: 0.25}, {Note: "F#4", Beats: 0.25}}
	want := int(math.Round(1.0 * 60.0 / 120.0 * SampleRate * 2))
	for _, style := range []string{"hum", "chime", "pluck"} {
		pcm, err := SynthesizeTune(TuneSpec{Tempo: 120, Style: style, Notes: notes})
		if err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		if len(pcm) != want {
			t.Fatalf("%s length = %d, want %d", style, len(pcm), want)
		}
		if p := peak(pcm); p < 1000 || p > 32767 {
			t.Fatalf("%s peak = %d", style, p)
		}
	}
}

func TestSynthesizeTuneBounds(t *testing.T) {
	ok := []TuneNote{{Note: "C4", Beats: 1}}
	bad := []struct {
		name string
		spec TuneSpec
	}{
		{"empty", TuneSpec{Notes: nil}},
		{"low tempo", TuneSpec{Tempo: 39, Notes: ok}},
		{"high tempo", TuneSpec{Tempo: 241, Notes: ok}},
		{"bad style", TuneSpec{Style: "drum", Notes: ok}},
		{"short beat", TuneSpec{Notes: []TuneNote{{Note: "C4", Beats: 0.2}}}},
		{"long beat", TuneSpec{Notes: []TuneNote{{Note: "C4", Beats: 9}}}},
		{"bad note", TuneSpec{Notes: []TuneNote{{Note: "H4", Beats: 1}}}},
	}
	for _, tc := range bad {
		if _, err := SynthesizeTune(tc.spec); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
	}
	many := make([]TuneNote, TuneMaxNotes+1)
	for i := range many {
		many[i] = TuneNote{Note: "C4", Beats: 1}
	}
	if _, err := SynthesizeTune(TuneSpec{Tempo: 240, Notes: many}); err == nil {
		t.Fatal("expected too-many-notes error")
	}
	long := make([]TuneNote, 16)
	for i := range long {
		long[i] = TuneNote{Note: "C4", Beats: 8}
	}
	if _, err := SynthesizeTune(TuneSpec{Tempo: 40, Notes: long}); err == nil {
		t.Fatal("expected duration error")
	}
}

func TestMetronomeBounds(t *testing.T) {
	for _, tc := range [][3]int{{29, 4, 5}, {241, 4, 5}, {100, 13, 5}, {100, 4, 4}, {100, 4, 301}} {
		if _, err := NewMetronomeSpec(tc[0], tc[1], tc[2]); err == nil {
			t.Fatalf("NewMetronomeSpec(%v) succeeded", tc)
		}
	}
	spec, err := NewMetronomeSpec(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if spec.BPM != 100 || spec.Beats != 4 || spec.DurationS != 120 {
		t.Fatalf("defaults = %+v", spec)
	}
}
