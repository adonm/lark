package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"

	"lark/internal/audio"
)

type replayClause struct {
	gap  int
	text string
}

type replayFixture struct {
	name, kind string
	voice      int
	clauses    []replayClause
}

var endpointFixtures = []replayFixture{
	{"timer", "short", 2, []replayClause{{0, "Start a timer for ninety seconds."}}},
	{"joke-name", "short", 3, []replayClause{{0, "Tell me a joke about Saoirse."}}},
	{"knock-knock", "short", 18, []replayClause{{0, "Tell me a knock knock joke!"}}},
	{"dragon-color", "short", 2, []replayClause{{0, "Make my dragon purple, please."}}},
	{"count", "short", 3, []replayClause{{0, "Count backwards from ten."}}},
	{"cat", "short", 18, []replayClause{{0, "Pretend to be a cat."}}},
	{"two-words", "short", 2, []replayClause{{0, "What's eight?"}}},
	{"moon", "short", 3, []replayClause{{0, "Why is the moon following us?"}}},
	{"whales", "short", 18, []replayClause{{0, "Do whales sleep underwater?"}}},
	{"dino", "short", 2, []replayClause{{0, "How big was a baby dinosaur?"}}},
	{"rainbow", "short", 3, []replayClause{{0, "Where does the rainbow end?"}}},
	{"calculator", "short", 18, []replayClause{{0, "What is seventeen plus thirty-two?"}}},
	{"snack", "short", 2, []replayClause{{0, "Can a dog eat popcorn?"}}},
	{"game", "short", 3, []replayClause{{0, "Let's play twenty questions!"}}},
	{"jupiter", "short", 18, []replayClause{{0, "Why does Jupiter have stripes?"}}},
	{"voice", "short", 2, []replayClause{{0, "Can you talk like a pirate?"}}},
	{"school", "short", 3, []replayClause{{0, "Where is the nearest school?"}}},
	{"frog-07", "pause", 2, []replayClause{{0, "Once upon a time there was a little frog, and then"}, {700, "it made a house under a purple mushroom."}}},
	{"frog-17", "pause", 2, []replayClause{{0, "Once upon a time there was a little frog, and then"}, {1700, "it made a house under a purple mushroom."}}},
	{"frog-32", "pause", 2, []replayClause{{0, "Once upon a time there was a little frog, and then"}, {3200, "it made a house under a purple mushroom."}}},
	{"frog-40", "pause", 2, []replayClause{{0, "Once upon a time there was a little frog, and then"}, {4000, "it made a house under a purple mushroom."}}},
	{"timer-pause", "pause", 3, []replayClause{{0, "Could you set a timer for"}, {2600, "seventeen minutes?"}}},
	{"joke-pause", "pause", 18, []replayClause{{0, "Tell me a joke about"}, {3600, "Siobhan and thirty-two spiders."}}},
	{"dragon-wings", "pause", 2, []replayClause{{0, "Can you draw a dragon"}, {1700, "with blue wings and little stars?"}}},
	{"article", "pause", 3, []replayClause{{0, "Tell me about the"}, {2800, "biggest volcano ever."}}},
	{"hesitation", "pause", 18, []replayClause{{0, "Can you make me a song, um"}, {3100, "about a fish who wears shoes?"}}},
	{"conditional", "pause", 2, []replayClause{{0, "If I had a million dollars"}, {2900, "could I buy the moon?"}}},
	{"sequential", "pause", 3, []replayClause{{0, "First I want a castle"}, {3900, "and then I want a moat with crocodiles."}}},
	{"list", "pause", 18, []replayClause{{0, "I want apples, grapes, and"}, {2200, "strawberries and"}, {1850, "chocolate sprinkles too!"}}},
	{"list-bare", "pause", 2, []replayClause{{0, "Red, orange, yellow"}, {1850, "green, blue, and purple."}}},
	{"name-recall", "pause", 3, []replayClause{{0, "That dinosaur is called"}, {3200, "Micropachycephalosaurus."}}},
	{"number-correction", "pause", 18, []replayClause{{0, "Set a timer for, um"}, {1400, "forty seconds! No, thirty."}}},
	{"filler-uh", "filler", 2, []replayClause{{0, "Tell me the dragon joke, uh."}}},
	{"filler-think", "filler", 3, []replayClause{{0, "Can we play a game, let me think."}}},
	{"filler-um", "filler", 18, []replayClause{{0, "Show me a giraffe, um."}}},
	{"purple-frog", "story", 2, []replayClause{
		{0, "Once upon a time there was a tiny frog who lived in a boot."},
		{3200, "His best friend had a purple umbrella."},
		{4000, "They used it like a boat when the creek got really big, and then"},
		{1500, "they met a turtle named Olivia who knew the way home."},
		{2400, "The frog gave her ninety stickers to say thank you. The end."},
	}},
	{"giant-bee", "story", 3, []replayClause{
		{0, "One day I saw a bee that was bigger than my hand."},
		{3870, "It was so sparkly I thought it might be a magic bee."},
		{1960, "It landed on my little brother's ice cream."},
		{3450, "He liked it because he thought the bee was a sprinkle."},
	}},
	{"space-dog", "story", 18, []replayClause{
		{0, "Let me tell you about my dog Rocket, who went in a spaceship."},
		{1450, "He had seven bags of treats but there was a hole in the door."},
		{4000, "So the treats all flew out and made a ring around Saturn."},
		{2450, "He was mad until a purple space cat shared her crackers."},
	}},
	{"backyard", "story", 2, []replayClause{
		{0, "Yesterday my cousin and I took a red shovel and a green bucket to the back corner of our yard because we wanted to dig for buried treasure."},
		{3800, "We found a very wobbly spoon and a marble and then"},
		{2850, "he said there was a noise under the rocks."},
		{4000, "It was a little lizard. We put the rocks back and left it alone."},
	}},
	{"telling-cat", "story", 3, []replayClause{
		{0, "This is a story about a cat named Nori."},
		{4000, "Nori could only jump nine inches, but she wanted to jump to the moon."},
		{1000, "The other cats made a trampoline out of an old blanket, and"},
		{3500, "Nori bounced high enough to catch a falling star."},
	}},
}

type replaySpan struct{ start, end int }

const replayBytesPerMS = sampleRate / 1000 * 2

// Run explicitly: all audio is synthesized and kept in memory.
func TestEndpointReplay(t *testing.T) {
	t.Parallel()
	path := os.Getenv("LARK_REPLAY_SILERO")
	if path == "" {
		t.Skip("set LARK_REPLAY_SILERO to run local synthetic shadow replay")
	}
	if err := checkEndpointModel(path); err != nil {
		t.Fatal(err)
	}
	model := os.Getenv("LARK_BAKEOFF_MODEL")
	if err := audio.CheckParakeetModel(model); err != nil {
		t.Fatal(err)
	}
	parakeet, err := audio.OpenParakeet(model)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(parakeet.Close)
	if _, err := parakeet.Transcribe(t.Context(), make([]byte, sampleRate*2)); err != nil {
		t.Fatal(err)
	}
	kokoro := os.Getenv("LARK_REPLAY_KOKORO")
	tts := sherpa.NewOfflineTts(&sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{Provider: "cpu", NumThreads: 2, Kokoro: sherpa.OfflineTtsKokoroModelConfig{
			Model: filepath.Join(kokoro, "model.onnx"), Voices: filepath.Join(kokoro, "voices.bin"),
			Tokens: filepath.Join(kokoro, "tokens.txt"), DataDir: filepath.Join(kokoro, "espeak-ng-data"), Lang: "en-us", LengthScale: 1,
		}}, MaxNumSentences: 1,
	})
	if tts == nil {
		t.Fatal("set LARK_REPLAY_KOKORO to the local Kokoro model directory")
	}
	defer sherpa.DeleteOfflineTts(tts)
	type score struct {
		early, missed, probes int
		total, short          []int
	}
	methods := []struct {
		name   string
		legacy bool
		board  bool
		hybrid bool
	}{
		{name: "board-rms", legacy: true, board: true},
		{name: "browser-rms", legacy: true},
		{name: "before/silero-no-text"},
		{name: "after/transcript-progress", hybrid: true},
	}
	scores := make([]score, len(methods))
	for _, f := range endpointFixtures {
		pcm := make([]byte, 250*replayBytesPerMS)
		var spans []replaySpan
		for _, part := range f.clauses {
			pcm = append(pcm, make([]byte, part.gap*replayBytesPerMS)...)
			start := len(pcm) / replayBytesPerMS
			pcm = append(pcm, replaySpeak(t, tts, part.text, f.voice)...)
			spans = append(spans, replaySpan{start, len(pcm) / replayBytesPerMS})
		}
		trueEnd := spans[len(spans)-1].end
		pcm = append(pcm, make([]byte, 6000*replayBytesPerMS)...)
		for i, m := range methods {
			s := &scores[i]
			stop := func(clip []byte) int {
				if m.legacy {
					return replayLegacy(clip, m.board)
				}
				at, probes := replaySilero(t, clip, path, m.hybrid, parakeet)
				s.probes += probes
				return at
			}
			at := stop(pcm)
			switch {
			case at < 0:
				s.missed++
			case at < trueEnd:
				s.early++
				t.Logf("%s %s premature by %d ms", m.name, f.name, trueEnd-at)
			default:
				delay := at - trueEnd
				s.total = append(s.total, delay)
				if f.kind == "short" {
					s.short = append(s.short, delay)
				}
			}
			if f.kind == "story" {
				fragments := 0
				// Re-arm before the next remaining clause, as if the speaker tapped
				// Talk again immediately. This is a score, not production behavior.
				for at >= 0 {
					fragments++
					if at >= trueEnd {
						break
					}
					k := slices.IndexFunc(spans, func(span replaySpan) bool { return span.end > at })
					start := max(at, spans[k].start)
					clip := append(make([]byte, 250*replayBytesPerMS), pcm[start*replayBytesPerMS:]...)
					next := stop(clip)
					if next <= 250 {
						at = -1
					} else {
						at = start + next - 250
					}
				}
				t.Logf("%s story=%s fragments=%d", m.name, f.name, fragments)
			}
		}
	}
	for i, s := range scores {
		t.Logf("%s premature=%d/%d missed=%d end_to_commit_ms=(%s) short_ms=(%s) probes=%d",
			methods[i].name, s.early, len(endpointFixtures), s.missed, replayPercentiles(s.total), replayPercentiles(s.short), s.probes)
	}
	if s := scores[len(scores)-1]; s.early != 0 || s.missed != 0 {
		t.Errorf("tuned policy did not capture all fixtures: %d cutoffs, %d missed", s.early, s.missed)
	}
}

func replaySpeak(t *testing.T, tts *sherpa.OfflineTts, text string, voice int) []byte {
	t.Helper()
	a := tts.Generate(text, voice, 1)
	if a == nil || a.SampleRate != 24000 {
		t.Fatal("fixture synthesis failed")
	}
	var pcm []byte
	for k := 0; k+2 < len(a.Samples); k += 3 {
		pcm = le.AppendUint16(pcm, uint16(int16(a.Samples[k]*32767)))
		pcm = le.AppendUint16(pcm, uint16(int16((a.Samples[k+1]+a.Samples[k+2])*16383)))
	}
	// Trim variable silent edges at 10 ms resolution, with 30 ms lead-in.
	const tick = endpointBytes / 2
	start, end := 0, 0
	for i := 0; i+tick <= len(pcm); i += tick {
		var energy int64
		for j := i; j < i+tick; j += 2 {
			x := int64(int16(le.Uint16(pcm[j:])))
			energy += x * x
		}
		if energy > tick/2*240*240 {
			if end == 0 {
				start = max(0, i-3*tick)
			}
			end = i + tick
		}
	}
	if end == 0 {
		t.Fatal("empty fixture")
	}
	return pcm[start:end]
}

func replaySilero(t *testing.T, pcm []byte, path string, hybrid bool, stt *audio.STT) (int, int) {
	t.Helper()
	vad := newSileroVAD(path)
	if vad == nil {
		t.Fatal("Silero unavailable")
	}
	defer vad.close()
	var p endpointPolicy
	lastProbeAt := -endpointProbeEvery
	var lastGateOpenAt time.Duration
	var probes int
	for i := endpointBytes; i <= len(pcm); i += endpointBytes {
		at := time.Duration(i/endpointBytes) * endpointFrame
		frame := pcm[i-endpointBytes : i]
		p.observeEnergy(at, vad.detect(frame), frameRMS(frame))
		if p.inVoice {
			lastGateOpenAt = at
		}
		if hybrid && p.voiced && at-lastProbeAt >= endpointProbeEvery && (p.inVoice || lastGateOpenAt > lastProbeAt) {
			lastProbeAt = at
			windowed := i > endpointProbeWindow
			startOff := max(0, i-endpointProbeWindow)
			text, err := stt.Transcribe(t.Context(), pcm[startOff:i])
			if err != nil {
				t.Fatal(err)
			}
			probes++
			p.observeProgress(at, text, windowed)
		}
		switch p.decision(at, endpointCap, endpointLimit, false, false) {
		case endpointCommit:
			return int(at.Milliseconds()), probes
		case endpointDiscard:
			return -1, probes
		}
	}
	return -1, probes
}

func replayPercentiles(v []int) string {
	if len(v) == 0 {
		return "n/a"
	}
	slices.Sort(v)
	return fmt.Sprintf("n=%d p50=%d p95=%d", len(v), v[(len(v)*50+99)/100-1], v[(len(v)*95+99)/100-1])
}

// Reproduce firmware/main/vad.c or web/mic.js using the replay audio clock.
func replayLegacy(pcm []byte, board bool) int {
	tick, calibration, floorN, startWait, short, long, longVoice := 20, 10, 3, 400, 75, 150, 200
	if board {
		tick, calibration, floorN, startWait, short, long, longVoice = 10, 20, 4, 700, 150, 300, 400
	}
	size := sampleRate * tick / 1000 * 2
	var quiet []float64
	var threshold float64
	voice, streak, silence := 0, 0, 0
	for i, frames := size, 1; i <= len(pcm); i, frames = i+size, frames+1 {
		var sum float64
		for j := i - size; j < i; j += 2 {
			x := float64(int16(le.Uint16(pcm[j:])))
			sum += x * x
		}
		rms := math.Sqrt(sum / float64(size/2))
		hear := func(level float64) {
			if voice > 0 && level >= threshold/2 || voice == 0 && level >= threshold {
				voice++
				silence = 0
			} else if voice > 0 {
				silence++
			}
		}
		switch {
		case frames <= calibration:
			quiet = append(quiet, rms)
			if frames == calibration {
				sorted := slices.Clone(quiet)
				slices.Sort(sorted)
				var avg float64
				for _, r := range sorted[:floorN] {
					if board {
						r *= r
					}
					avg += r
				}
				avg /= float64(floorN)
				if board {
					threshold = min(2400, max(320, math.Sqrt(avg)*2.5))
				} else {
					threshold = max(327.68, avg*2.5)
					for _, r := range quiet {
						hear(r)
					}
				}
			}
		case board && voice == 0:
			if rms >= threshold {
				streak++
			} else {
				streak = 0
			}
			if streak >= 3 {
				voice = streak
			}
		default:
			hear(rms)
		}
		if frames*tick >= 60000 || voice == 0 && frames >= startWait ||
			voice < longVoice && silence >= short || voice >= longVoice && silence >= long {
			return frames * tick
		}
	}
	return -1
}
