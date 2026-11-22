//go:build live

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSmartTurnAccuracyAndLatency(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	modelPath := filepath.Join(data, "lark", "stt-models", smartTurnModel)
	kokoroDir := filepath.Join(data, "lark", "tts-models", kokoroPack)

	eng, err := openSmartTurn(modelPath, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.close()

	k, err := openKokoro(kokoroDir, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer k.close()

	type sample struct {
		label    string
		text     string
		complete bool
	}
	cases := []sample{
		// Complete questions & statements & short answers
		{"q_math", "What is two plus two?", true},
		{"q_moon", "How far away is the moon?", true},
		{"q_sky", "Why is the sky blue?", true},
		{"q_story", "Can you tell me a story about a fox?", true},
		{"cmd_timer", "Set a timer for five minutes.", true},
		{"stmt_fact", "Tell me a fun fact about octopuses.", true},
		{"stmt_dragon", "I like purple dragons.", true},
		{"short_yes", "Yes please.", true},
		{"short_ok", "Okay, sounds good.", true},
		{"short_no", "No thank you.", true},
		{"stmt_cat", "My cat is sleeping on the sofa.", true},
		{"q_who", "Who built the first rocket?", true},
		{"num_complete", "The answer is forty-two.", true},
		{"list_complete", "I like apples, bananas, and oranges.", true},
		// Incomplete: trailing fillers, mid-sentence pauses, lists, numbers
		{"inc_um", "I want to ask about, um,", false},
		{"inc_uh", "Let me think, uh,", false},
		{"inc_so_um", "So yesterday I, um,", false},
		{"inc_about", "Can you tell me a story about", false},
		{"inc_and", "I went to the park and", false},
		{"inc_if", "If we go outside tomorrow,", false},
		{"inc_was_a", "The first thing I saw was a", false},
		{"inc_because", "I stayed inside because", false},
		{"inc_list1", "I need apples, bananas,", false},
		{"inc_list2", "Red, blue, green, and", false},
		{"inc_num1", "One, two, three,", false},
		{"inc_num2", "The code is five, seven,", false},
	}

	// 200ms trailing silence (matches the 200ms pause check in endpointing).
	silenceTail := make([]byte, sampleRate*2*200/1000)

	type result struct {
		sample
		prob float32
		dur  time.Duration
	}
	var results []result
	for _, tc := range cases {
		f32, _ := k.generate(tc.text, voiceCatalog[0], func() bool { return true })
		if len(f32) == 0 {
			t.Fatalf("kokoro failed for %q", tc.text)
		}
		var pcm []byte
		for i := 0; i+2 < len(f32); i += 3 {
			pcm = le.AppendUint16(pcm, uint16(int16(f32[i]*32767)))
			pcm = le.AppendUint16(pcm, uint16(int16((f32[i+1]+f32[i+2])*16383)))
		}
		// Trim Kokoro's trailing silence so we append an exact 200ms pause.
		const tick = endpointBytes / 2
		end := len(pcm)
		for i := len(pcm) - tick; i >= 0; i -= tick {
			var energy int64
			for j := i; j < i+tick; j += 2 {
				x := int64(int16(le.Uint16(pcm[j:])))
				energy += x * x
			}
			if energy > tick/2*240*240 {
				end = min(len(pcm), i+tick)
				break
			}
		}
		pcm = append(pcm[:end], silenceTail...)
		start := time.Now()
		prob, err := eng.predictPCM16(pcm)
		dur := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result{tc, prob, dur})
		t.Logf("%-14s complete=%-5v prob=%.4f dur=%s (%q)", tc.label, tc.complete, prob, dur.Round(time.Millisecond), tc.text)
	}

	// Warm latency
	var totalDur time.Duration
	for _, r := range results[1:] {
		totalDur += r.dur
	}
	t.Logf("mean warm inference (4 threads): %s", (totalDur / time.Duration(len(results)-1)).Round(time.Millisecond))

	for _, thresh := range []float32{0.50, 0.70, 0.80, 0.85, 0.90, 0.95} {
		var fp, fn, nInc, nComp int
		for _, r := range results {
			pred := r.prob >= thresh
			if r.complete {
				nComp++
				if !pred {
					fn++
				}
			} else {
				nInc++
				if pred {
					fp++
				}
			}
		}
		fmt.Printf("thresh=%.2f: false_commit=%d/%d (%.1f%%) missed_complete=%d/%d (%.1f%%)\n",
			thresh, fp, nInc, 100*float64(fp)/float64(nInc), fn, nComp, 100*float64(fn)/float64(nComp))
	}
}
