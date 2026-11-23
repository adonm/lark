//go:build live

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTurnBench(t *testing.T) {
	if os.Getenv("LARK_TURN_BENCH") == "" {
		t.Skip("set LARK_TURN_BENCH=1 to run the live turn benchmark")
	}
	// Default to the dedicated real model; set LARK_TURN_BENCH_MODE=stub to
	// isolate the server pipeline from model latency.
	s := startE2E(t, env("LARK_TURN_BENCH_MODE", "real") != "stub")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	engine, err := openKokoro(ttsModelDir(filepath.Join(data, "lark")), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.close()

	utterances := []struct {
		name string
		text string
	}{
		{"chat", "What is two plus two?"},
		{"timer", "Set a timer for two minutes."},
		{"reason", "Why is the sky blue?"},
	}
	clips := make([][]byte, len(utterances))
	silence := make([]byte, sampleRate*2*2500/1000) // 2.5 s trailing silence for endpointing
	for i, u := range utterances {
		pcm, err := synthesizePCM(t.Context(), engine, heartVoice, u.text)
		if err != nil {
			t.Fatalf("synthesize %q: %v", u.text, err)
		}
		clip := append(pcm, silence...)
		if rem := len(clip) % rtpPCMBytes; rem != 0 {
			clip = append(clip, make([]byte, rtpPCMBytes-rem)...)
		}
		clips[i] = clip
	}

	c := s.board(t, "esp-vocat", "Bench")
	c.timeout = 45 * time.Second
	defer func() {
		if t.Failed() {
			r := liveJSON[diagReport](t, s.liveClient, "/api/diagnostics", nil, nil, "", nil)
			for i, tr := range r.Turns {
				f := func(p *float64) string {
					if p == nil {
						return "-"
					}
					return fmt.Sprintf("%.1f", *p)
				}
				t.Logf("turn[%d]: src=%s stt=%s llm1=%s llmtot=%s tts1=%s pcm1=%s e2e=%s",
					i, tr.Source, f(tr.STT), f(tr.First), f(tr.Total), f(tr.TTS), f(tr.FirstPCM), f(tr.E2E))
			}
		}
	}()

	const defaultRuns = 3
	runs := defaultRuns
	if raw := os.Getenv("LARK_TURN_BENCH_RUNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 20 {
			t.Fatalf("LARK_TURN_BENCH_RUNS=%q: expected 1..20", raw)
		}
		runs = n
	}
	var labels []string
	var seq uint16
	for run := 1; run <= runs; run++ {
		for idx, u := range utterances {
			labels = append(labels, fmt.Sprintf("r%d-%s", run, u.name))
			c.syncFrames(t)
			c.input(t, "tap", 180, 140, c.revision)
			c.expect(t, "rec start", func(f protocolFrame) bool {
				return e2eFields(f, "rec", "action", `"start"`)
			})
			c.recStart(t, "")
			pcm := clips[idx]
			ticker := time.NewTicker(20 * time.Millisecond)
			stopped := false
			for offset := 0; offset < len(pcm) && !stopped; offset += rtpPCMBytes {
				part := pcm[offset:min(offset+rtpPCMBytes, len(pcm))]
				seq++
				c.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, part)
				<-ticker.C
			}
			ticker.Stop()
			c.expect(t, "rec stop", func(f protocolFrame) bool {
				return e2eFields(f, "rec", "action", `"stop"`)
			})
			c.recEnd(t, "", false)
			c.expect(t, "tts start", func(f protocolFrame) bool {
				return e2eFields(f, "tts", "state", `"start"`)
			})
			c.expect(t, "first tts pcm", func(f protocolFrame) bool {
				if f.op != wsBinary {
					return false
				}
				mt, _, _, ok := unpackProtoPacket(f.data)
				return ok && mt == msgTTSPCM
			})
			end := c.expect(t, "tts end", func(f protocolFrame) bool {
				return e2eFields(f, "tts", "state", `"end"`)
			})
			c.ack(t, end)
			// Let playbackDone settle before next recStart.
			time.Sleep(100 * time.Millisecond)
		}
	}

	report := liveJSON[diagReport](t, s.liveClient, "/api/diagnostics", nil, nil, "", nil)
	turns := report.Turns
	// report.Turns is newest-first; reverse to match execution order. A turn
	// with no transcript leaves no diagnostic row, so align the tail and warn.
	n := min(len(labels), len(turns))
	if n == 0 {
		t.Fatal("no diagnostic turns recorded")
	}
	if n < len(labels) {
		t.Logf("bench: %d of %d utterances produced turns; keeping the newest", n, len(labels))
		labels = labels[len(labels)-n:]
	}
	ordered := make([]diagTurn, n)
	for i := range ordered {
		ordered[i] = turns[n-1-i]
	}
	byKind := map[string][]float64{}
	for i, tr := range ordered {
		kind := labels[i]
		if _, rest, ok := strings.Cut(kind, "-"); ok {
			kind = rest
		}
		if tr.E2E != nil {
			byKind[kind] = append(byKind[kind], *tr.E2E)
		}
	}
	for kind, vals := range byKind {
		slices.Sort(vals)
		p := func(q int) float64 { return vals[(len(vals)*q+99)/100-1] }
		t.Logf("bench %s: n=%d e2e p50=%.0fms p95=%.0fms", kind, len(vals), p(50), p(95))
	}

	fmtMS := func(v *float64) string {
		if v == nil {
			return "-"
		}
		return fmt.Sprintf("%.1f", *v)
	}
	var sb strings.Builder
	sb.WriteString("\nTurn       | endpt  | vad_q | stt_q | stt_r | stt    | prep  | subm  | llm_1st | 1st_sent | tts_q | tts_inf | resamp | tts_1st | 1st_pcm | e2e\n")
	sb.WriteString("-----------|--------|-------|-------|-------|--------|-------|-------|---------|----------|-------|---------|--------|---------|---------|-------\n")
	for i, tr := range ordered {
		fmt.Fprintf(&sb, "%-10s | %6s | %5s | %5s | %5s | %6s | %5s | %5s | %7s | %8s | %5s | %7s | %6s | %7s | %7s | %6s\n",
			labels[i],
			fmtMS(tr.Endpoint), fmtMS(tr.VADQueue),
			fmtMS(tr.STTQueue), fmtMS(tr.STTRun), fmtMS(tr.STT),
			fmtMS(tr.PromptPrep), fmtMS(tr.PromptSubmit),
			fmtMS(tr.First), fmtMS(tr.FirstSentence),
			fmtMS(tr.TTSQueue), fmtMS(tr.TTSInfer), fmtMS(tr.Resample),
			fmtMS(tr.TTS), fmtMS(tr.FirstPCM), fmtMS(tr.E2E))
	}
	sp := report.Speculation
	t.Logf("speculation: started=%d hit=%d abort_words=%d abort_speech=%d abort_mismatch=%d abort_other=%d overlap_avg=%.0fms",
		sp.Started, sp.Hit, sp.AbortWords, sp.AbortSpeech, sp.AbortMismatch, sp.AbortOther, sp.OverlapAvgMS)
	if f := report.Latency["spec_first_ms"]; f.Count > 0 && f.P50 != nil && f.P95 != nil {
		t.Logf("speculative TTFT: p50=%.0fms p95=%.0fms n=%d", *f.P50, *f.P95, f.Count)
	}
	t.Log(sb.String())
}
