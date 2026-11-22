//go:build live

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTurnBench(t *testing.T) {
	if os.Getenv("LARK_TURN_BENCH") == "" {
		t.Skip("set LARK_TURN_BENCH=1 to run the live turn benchmark")
	}
	s := startE2E(t, true)
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

	const runs = 3
	var labels []string
	var seq uint16
	for run := 1; run <= runs; run++ {
		for idx, u := range utterances {
			labels = append(labels, fmt.Sprintf("r%d-%s", run, u.name))
			c.syncFrames(t)
			c.input(t, "tap", 180, 140, c.revision)
			c.expect(t, "rec start", func(f wsFrame) bool {
				return e2eFields(f, "rec", "action", `"start"`)
			})
			c.recStart(t, "")
			pcm := clips[idx]
			ticker := time.NewTicker(20 * time.Millisecond)
			stopped := false
			for offset := 0; offset < len(pcm) && !stopped; offset += rtpPCMBytes {
				part := pcm[offset:min(offset+rtpPCMBytes, len(pcm))]
				seq++
				c.sendRTP(t, rtpPTRecPCM, seq, uint32(seq)*320, 1, part)
				<-ticker.C
			}
			ticker.Stop()
			c.expect(t, "rec stop", func(f wsFrame) bool {
				return e2eFields(f, "rec", "action", `"stop"`)
			})
			c.recEnd(t, "", false)
			c.expect(t, "tts start", func(f wsFrame) bool {
				return e2eFields(f, "tts", "state", `"start"`)
			})
			c.expect(t, "first tts pcm", func(f wsFrame) bool {
				if f.op != wsBinary {
					return false
				}
				mt, _, _, ok := unpackProtoPacket(f.data)
				return ok && mt == msgTTSPCM
			})
			end := c.expect(t, "tts end", func(f wsFrame) bool {
				return e2eFields(f, "tts", "state", `"end"`)
			})
			c.ack(t, end)
			// Let playbackDone settle before next recStart.
			time.Sleep(100 * time.Millisecond)
		}
	}

	report := liveJSON[diagReport](t, s.liveClient, "/api/diagnostics", nil, nil, "", nil)
	turns := report.Turns
	// report.Turns is newest-first; reverse to match execution order.
	if len(turns) < len(labels) {
		t.Fatalf("got %d diag turns, want at least %d", len(turns), len(labels))
	}
	ordered := make([]diagTurn, len(labels))
	for i := range labels {
		ordered[i] = turns[len(labels)-1-i]
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
	t.Log(sb.String())
}
