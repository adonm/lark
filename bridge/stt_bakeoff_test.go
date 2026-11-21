//go:build linux

package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

func TestSTTBakeoff(t *testing.T) {
	mode := os.Getenv("LARK_BAKEOFF")
	if mode == "" {
		t.Skip("set LARK_BAKEOFF and LARK_BAKEOFF_AUDIO to run the local bake-off")
	}
	if mode == "fixtures" {
		generateBakeoffAudio(t)
		return
	}
	var audio [][]byte
	files, err := filepath.Glob(filepath.Join(os.Getenv("LARK_BAKEOFF_AUDIO"), "*.wav"))
	if err != nil || len(files) == 0 {
		t.Fatal("supply a directory of 16 kHz mono PCM WAV files")
	}
	for _, file := range files {
		a := sherpa.ReadWave(file)
		if a == nil || a.SampleRate != sampleRate {
			t.Fatalf("unreadable 16 kHz WAV: %s", filepath.Base(file))
		}
		pcm := make([]byte, 0, 2*len(a.Samples))
		for _, v := range a.Samples {
			pcm = le.AppendUint16(pcm, uint16(int16(v*32768)))
		}
		audio = append(audio, pcm)
	}
	threads, _ := strconv.Atoi(env("LARK_BAKEOFF_THREADS", "8"))
	model := os.Getenv("LARK_BAKEOFF_MODEL")
	var transcribe func(context.Context, []byte) (string, error)
	var stream func(context.Context, []byte) (time.Duration, time.Duration, time.Duration, error)
	initStart := time.Now()
	switch mode {
	case "sherpa":
		transcribe = bakeoffSherpa(t, model, threads)
	case "parakeet":
		threads = 1
		p, err := openParakeet(model)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.close)
		transcribe = p.transcribe
	case "sherpa-stream":
		recognizer := sherpa.NewOnlineRecognizer(&sherpa.OnlineRecognizerConfig{
			FeatConfig: sherpa.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
			ModelConfig: sherpa.OnlineModelConfig{
				Transducer: sherpa.OnlineTransducerModelConfig{
					Encoder: filepath.Join(model, "encoder.int8.onnx"),
					Decoder: filepath.Join(model, "decoder.int8.onnx"),
					Joiner:  filepath.Join(model, "joiner.int8.onnx"),
				},
				Tokens: filepath.Join(model, "tokens.txt"), NumThreads: threads, Provider: "cpu",
			},
			DecodingMethod: "greedy_search",
		})
		if recognizer == nil {
			t.Fatal("could not create online recognizer")
		}
		t.Cleanup(func() { sherpa.DeleteOnlineRecognizer(recognizer) })
		stream = func(_ context.Context, pcm []byte) (time.Duration, time.Duration, time.Duration, error) {
			return bakeoffSherpaStream(recognizer, pcm)
		}
	default:
		t.Fatalf("unknown bake-off mode %q", mode)
	}
	init := time.Since(initStart)
	pid := os.Getpid()
	var first, final, flush []int
	var totalWork time.Duration
	if transcribe != nil {
		ctx, stop := context.WithTimeout(t.Context(), 40*time.Second)
		start := time.Now()
		text, err := transcribe(ctx, audio[0])
		stop()
		if err != nil || text == "" {
			t.Fatalf("first read: missing transcript (%v)", err)
		}
		t.Logf("init_ms=%d first_clip_ms=%d", init.Milliseconds(), time.Since(start).Milliseconds())
	}
	cpuStart := bakeoffCPU(t, pid)
	for i, pcm := range audio {
		ctx, stop := context.WithTimeout(t.Context(), 40*time.Second)
		start := time.Now()
		if transcribe != nil {
			text, err := transcribe(ctx, pcm)
			stop()
			if err != nil || text == "" {
				t.Fatalf("clip %d: missing transcript (%v)", i+1, err)
			}
			elapsed := time.Since(start)
			totalWork += elapsed
			final = append(final, int(elapsed.Milliseconds()))
		} else {
			f, end, finish, err := stream(ctx, pcm)
			stop()
			if err != nil {
				t.Fatalf("stream %d: %v", i+1, err)
			}
			if f > 0 {
				first = append(first, int(f.Milliseconds()))
			}
			final = append(final, int(end.Milliseconds()))
			flush = append(flush, int(finish.Milliseconds()))
			totalWork += time.Since(start)
			if i == 0 {
				t.Logf("init_ms=%d first_run_partial_ms=%d first_run_final_ms=%d", init.Milliseconds(), f.Milliseconds(), end.Milliseconds())
			}
		}
	}
	var memory string
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err == nil {
		for line := range strings.SplitSeq(string(status), "\n") {
			if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") {
				memory += strings.Join(strings.Fields(line), "") + " "
			}
		}
	}
	t.Logf("mode=%s threads=%d first_partial_ms=(%s) final_ms=(%s) flush_ms=(%s) cpu_pct=%.0f %s%s",
		mode, threads, replayPercentiles(first), replayPercentiles(final), replayPercentiles(flush),
		100*(bakeoffCPU(t, pid)-cpuStart).Seconds()/totalWork.Seconds(), memory, bakeoffVRAM(pid))
}

func bakeoffCPU(t *testing.T, pid int) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

func bakeoffVRAM(pid int) string {
	files, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fdinfo/*", pid))
	clients := make(map[string]int64)
	for _, path := range files {
		raw, _ := os.ReadFile(path)
		var client string
		var resident int64
		for line := range strings.SplitSeq(string(raw), "\n") {
			if value, ok := strings.CutPrefix(line, "drm-client-id:"); ok {
				client = strings.TrimSpace(value)
			}
			if value, ok := strings.CutPrefix(line, "drm-memory-vram:"); ok {
				fmt.Sscan(value, &resident) // KiB, as reported by amdgpu.
			}
		}
		if client != "" {
			clients[client] = max(resident, clients[client])
		}
	}
	var total int64
	for _, kiB := range clients {
		total += kiB
	}
	return fmt.Sprintf("vram_kib=%d", total)
}

func bakeoffSherpa(t *testing.T, dir string, threads int) func(context.Context, []byte) (string, error) {
	t.Helper()
	ext := env("LARK_BAKEOFF_ONNX_EXT", ".int8.onnx")
	r := sherpa.NewOfflineRecognizer(&sherpa.OfflineRecognizerConfig{
		FeatConfig: sherpa.FeatureConfig{SampleRate: sampleRate, FeatureDim: 80},
		ModelConfig: sherpa.OfflineModelConfig{
			Transducer: sherpa.OfflineTransducerModelConfig{
				Encoder: filepath.Join(dir, "encoder"+ext),
				Decoder: filepath.Join(dir, "decoder"+ext),
				Joiner:  filepath.Join(dir, "joiner"+ext),
			},
			Tokens: filepath.Join(dir, "tokens.txt"), NumThreads: threads, Provider: "cpu",
		},
		DecodingMethod: "greedy_search",
	})
	if r == nil {
		t.Fatal("could not create offline recognizer")
	}
	t.Cleanup(func() { sherpa.DeleteOfflineRecognizer(r) })
	return func(_ context.Context, pcm []byte) (string, error) {
		s := sherpa.NewOfflineStream(r)
		defer sherpa.DeleteOfflineStream(s)
		s.AcceptWaveform(sampleRate, bakeoffSamples(pcm))
		r.Decode(s)
		return s.GetResult().Text, nil
	}
}

func bakeoffSamples(pcm []byte) []float32 {
	samples := make([]float32, len(pcm)/2)
	for i := range samples {
		samples[i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	return samples
}

func bakeoffSherpaStream(r *sherpa.OnlineRecognizer, pcm []byte) (first, final, flush time.Duration, err error) {
	start := time.Now()
	s := sherpa.NewOnlineStream(r)
	defer sherpa.DeleteOnlineStream(s)
	samples := bakeoffSamples(pcm)
	// Decode each arriving frame and include any backlog in wall-clock latency.
	for at := 0; at < len(samples); {
		end := min(len(samples), at+sampleRate/50)
		time.Sleep(time.Until(start.Add(time.Duration(end) * time.Second / sampleRate)))
		s.AcceptWaveform(sampleRate, samples[at:end])
		for r.IsReady(s) {
			r.Decode(s)
			if first == 0 && strings.TrimSpace(r.GetResult(s).Text) != "" {
				first = time.Since(start)
			}
		}
		at = end
	}
	flushStart := time.Now()
	// The final chunk needs right context even after InputFinished.
	s.AcceptWaveform(sampleRate, make([]float32, sampleRate*14/10))
	s.InputFinished()
	for r.IsReady(s) {
		r.Decode(s)
	}
	if strings.TrimSpace(r.GetResult(s).Text) == "" {
		return 0, 0, 0, fmt.Errorf("missing final")
	}
	return first, time.Since(start) - time.Duration(len(samples))*time.Second/sampleRate, time.Since(flushStart), nil
}

func generateBakeoffAudio(t *testing.T) {
	dir := os.Getenv("LARK_BAKEOFF_AUDIO")
	k := os.Getenv("LARK_REPLAY_KOKORO")
	if dir == "" || k == "" {
		t.Fatal("set LARK_BAKEOFF_AUDIO (scratch output) and LARK_REPLAY_KOKORO")
	}
	r := sherpa.NewOfflineTts(&sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{Provider: "cpu", NumThreads: 2, Kokoro: sherpa.OfflineTtsKokoroModelConfig{
			Model: filepath.Join(k, "model.onnx"), Voices: filepath.Join(k, "voices.bin"), Tokens: filepath.Join(k, "tokens.txt"),
			DataDir: filepath.Join(k, "espeak-ng-data"), Lang: "en-us", LengthScale: 1,
		}}, MaxNumSentences: 1,
	})
	if r == nil {
		t.Fatal("Kokoro unavailable")
	}
	defer sherpa.DeleteOfflineTts(r)
	for i, text := range []string{
		"Could you tell me a joke about a tiny frog?",
		"Start a timer for ninety seconds.",
		"Can you draw a dragon with blue wings and little stars around it?",
		"I saw a green frog in the garden and then it jumped into my shoe. Now it won't come out.",
		"Can we change the cat's eyes to purple hearts and give it a hat like the one I wore yesterday?",
		"Once upon a time there was a little frog who lived by a pond. Every morning it would hop across the road to visit a friend who lived under a purple mushroom.",
	} {
		a := r.Generate(text, []int{2, 18, 25}[i%3], 1)
		if a == nil || a.SampleRate != 24000 {
			t.Fatal("could not generate fixture")
		}
		pcm := make([]byte, 0, len(a.Samples)*4/3)
		for j := 0; j+2 < len(a.Samples); j += 3 {
			for _, v := range []float32{a.Samples[j], (a.Samples[j+1] + a.Samples[j+2]) / 2} {
				pcm = le.AppendUint16(pcm, uint16(int16(math.Round(float64(v*32767)))))
			}
		}
		path := filepath.Join(dir, fmt.Sprintf("fixture-%d.wav", i))
		header := wavHeader(len(pcm))
		if err := os.WriteFile(path, append(header[:], pcm...), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture=%d duration_ms=%d", i, len(pcm)/32)
	}
}
