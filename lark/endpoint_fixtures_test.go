package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Voice replay fixtures. The committed set is synthetic TTS audio only; a
// developer may drop real captures into qa/fixtures/local (gitignored) with a
// manifest of the same shape to run their own endpoint shapes locally.

type voiceExpectation struct {
	Endpoint                string `json:"endpoint"`
	CommitAfterLastVoicedMS int    `json:"commit_after_last_voiced_ms"`
	Incomplete              bool   `json:"incomplete"`
	NoCommitMS              []int  `json:"no_commit_ms"`
	MinSpeechMS             int    `json:"min_speech_ms"`
}

type voiceProgress struct {
	AtMS int    `json:"at_ms"`
	Text string `json:"text"`
}

type voiceFixture struct {
	Name       string           `json:"name"`
	File       string           `json:"file"`
	Origin     string           `json:"origin"`
	Kind       string           `json:"kind"`
	MS         int              `json:"ms"`
	Transcript string           `json:"transcript"`
	Progress   []voiceProgress  `json:"progress"`
	Expected   voiceExpectation `json:"expected"`
}

type voiceManifest struct {
	SampleRate int            `json:"sample_rate"`
	Fixtures   []voiceFixture `json:"fixtures"`
}

func readVoiceManifest(t *testing.T, dir string) voiceManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("voice fixture manifest: %v", err)
	}
	var m voiceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("voice fixture manifest: %v", err)
	}
	if m.SampleRate != sampleRate {
		t.Fatalf("voice fixtures are %d Hz, bridge is %d Hz", m.SampleRate, sampleRate)
	}
	return m
}

type fixtureRun struct {
	commitAt    time.Duration
	commitAfter time.Duration
	lastVoiced  time.Duration
	firstVoiced time.Duration
	speech      time.Duration
	voiced      bool
	incomplete  bool
	discard     bool
}

// replayVoiceFixture drives the production endpoint policy with the fixture's
// real audio and a fake VAD that calls frames with RMS >= 220 speech. Smart
// Turn is simulated as confident so the quiet-commit guard is exercised; the
// Silero/Smart Turn models themselves are not under test here.
func replayVoiceFixture(t *testing.T, pcm []byte, progress []voiceProgress) fixtureRun {
	t.Helper()
	var p endpointPolicy
	var run fixtureRun
	nextProgress := 0
	for i := endpointBytes; i <= len(pcm); i += endpointBytes {
		at := time.Duration(i/endpointBytes) * endpointFrame
		frame := pcm[i-endpointBytes : i]
		rms := frameRMS(frame)
		p.observeEnergy(at, rms >= 220, rms)
		if p.inVoice && run.lastVoiced < at {
			run.lastVoiced = at
		}
		for nextProgress < len(progress) && at.Milliseconds() >= int64(progress[nextProgress].AtMS) {
			p.observeProgress(at, progress[nextProgress].Text, false)
			nextProgress++
		}
		if run.commitAt == 0 {
			switch p.decide(at, endpointCap, endpointLimit, 0.99, false, false) {
			case endpointCommit:
				run.commitAt = at
				run.commitAfter = at - p.lastVoicedAt
			case endpointDiscard:
				run.discard = true
			}
		}
	}
	run.firstVoiced, run.speech = p.firstVoicedAt, p.speechSpan()
	run.voiced, run.incomplete = p.voiced, p.incomplete
	if run.commitAt == 0 {
		// A fixed listen window ends at its span; a tap that heard nothing
		// discards on the no-speech timeout.
		at := time.Duration(len(pcm)/endpointBytes) * endpointFrame
		if p.decide(at, endpointCap, at, 0.99, false, true) == endpointDiscard {
			run.discard = true
		}
	}
	return run
}

func TestVoiceFixtureManifestIsSynthetic(t *testing.T) {
	t.Parallel()
	m := readVoiceManifest(t, filepath.Join("qa", "fixtures", "voice"))
	if len(m.Fixtures) == 0 {
		t.Fatal("no committed voice fixtures")
	}
	dir := filepath.Join("qa", "fixtures", "voice")
	listed := map[string]bool{}
	var total int64
	for _, f := range m.Fixtures {
		if f.Origin != "synthetic" {
			// Real household audio must stay in the gitignored local directory.
			t.Fatalf("fixture %s has origin %q; only synthetic clips may be tracked", f.Name, f.Origin)
		}
		listed[f.File] = true
		info, err := os.Stat(filepath.Join(dir, f.File))
		if err != nil {
			t.Fatalf("fixture %s: %v", f.Name, err)
		}
		if info.Size() > 2<<20 {
			t.Fatalf("fixture %s is %d bytes; keep individual clips under 2 MiB", f.File, info.Size())
		}
		total += info.Size()
	}
	// An unlisted clip would be replayed by nothing and its provenance is
	// unknown, so every tracked voice file must come from the manifest.
	files, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if base := filepath.Base(path); !listed[base] {
			t.Errorf("voice fixture %s is not listed in manifest.json (missing origin)", base)
		}
	}
	if total > 5<<20 {
		t.Fatalf("committed voice fixtures are %d bytes; keep the set under 5 MiB", total)
	}
}

func TestVoiceFixturesEndpointPolicy(t *testing.T) {
	t.Parallel()
	manifests := []string{filepath.Join("qa", "fixtures", "voice")}
	if _, err := os.Stat(filepath.Join("qa", "fixtures", "local", "manifest.json")); err == nil {
		manifests = append(manifests, filepath.Join("qa", "fixtures", "local"))
	}
	for _, dir := range manifests {
		dir := dir
		t.Run(filepath.Base(dir), func(t *testing.T) {
			m := readVoiceManifest(t, dir)
			for _, f := range m.Fixtures {
				f := f
				t.Run(f.Name, func(t *testing.T) {
					t.Parallel()
					pcm, err := readWAVPCM16k(filepath.Join(dir, f.File))
					if err != nil {
						t.Fatalf("read fixture: %v", err)
					}
					run := replayVoiceFixture(t, pcm, f.Progress)
					t.Logf("%s: speech=%v first=%v last=%v commit=%v after=%v incomplete=%v",
						f.Kind, run.speech.Round(time.Millisecond), run.firstVoiced, run.lastVoiced,
						run.commitAt, run.commitAfter.Round(time.Millisecond), run.incomplete)

					if f.Expected.Incomplete && !run.incomplete {
						t.Fatalf("trailing word should count as incomplete")
					}
					if f.Expected.MinSpeechMS > 0 && run.speech < time.Duration(f.Expected.MinSpeechMS)*time.Millisecond {
						t.Fatalf("speech span %v is below the fixture's %d ms", run.speech, f.Expected.MinSpeechMS)
					}
					if f.Expected.Endpoint == "discard" {
						if run.voiced {
							t.Fatal("noise-only fixture opened a speech segment")
						}
						if !run.discard {
							t.Fatal("noise-only window did not discard")
						}
						return
					}
					if !run.voiced {
						t.Fatal("fixture did not open a segment")
					}
					if run.commitAt == 0 {
						t.Fatalf("fixture never committed (last voiced %v)", run.lastVoiced)
					}
					for _, at := range f.Expected.NoCommitMS {
						probe := time.Duration(at) * time.Millisecond
						if run.commitAt <= probe {
							t.Fatalf("committed at %v, inside the %d ms pause window", run.commitAt, at)
						}
					}
					if f.Expected.Endpoint == "monologue" && run.commitAt-run.firstVoiced < 19*time.Second {
						t.Fatalf("monologue committed after %v of continuous speech", run.commitAt-run.firstVoiced)
					}
					if want := time.Duration(f.Expected.CommitAfterLastVoicedMS) * time.Millisecond; want > 0 {
						if run.commitAfter != want {
							t.Fatalf("commit %v after last voiced, want %v", run.commitAfter, want)
						}
					}
				})
			}
		})
	}
}
