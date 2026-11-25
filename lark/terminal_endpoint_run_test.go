package main

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"
)

// A driven run needs no hub, server, or goroutine: packets go straight into the
// policy and every rule is observable from the run state.
func testEndpointRun(listen bool) *endpointRun {
	ctx, cancel := context.WithCancel(context.Background())
	c := &endpointCapture{
		ctx:        ctx,
		cancel:     cancel,
		listen:     listen,
		limitSpan:  endpointLimit,
		limitBytes: utteranceBytes(endpointLimit),
		result:     make(chan captureResult, 1),
	}
	r := &endpointRun{
		c:         c,
		vad:       &testEndpointVAD{},
		listen:    listen,
		limitSpan: endpointLimit,
		decode:    func(context.Context, []byte) (string, error) { return "", nil },
		// No Smart Turn result yet, as on a live run before its first check.
		smartProb: -1,
	}
	r.u.pcm = make([]byte, 0, sampleRate*2)
	r.u.limit = c.limitBytes
	return r
}

var (
	voicedEndpointFrame = func() []byte {
		b := make([]byte, endpointBytes)
		b[0], b[1] = 0xE8, 0x03 // 1000 RMS counts, above any gate margin
		return b
	}()
	silentEndpointFrame = make([]byte, endpointBytes)
)

func feedEndpoint(r *endpointRun, pcm []byte, at time.Time) {
	r.packet(micPacket{pcm: pcm, at: at})
}

func feedEndpointFrames(r *endpointRun, frame []byte, n int, at *time.Time) {
	for range n {
		feedEndpoint(r, frame, *at)
		*at = at.Add(endpointFrame)
	}
}

// The policy clock must include the audio a listening window's pre-roll ring
// discarded, otherwise a long idle window makes the monologue clock restart.
func TestEndpointRunPreRollKeepsClock(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(true)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, silentEndpointFrame, 500, &at) // 10 s of room tone
	feedEndpointFrames(r, voicedEndpointFrame, 2, &at)
	if r.u.policy.firstVoicedAt < 9*time.Second {
		t.Fatalf("first voiced frame at %v, want the pre-roll clock (~10 s)", r.u.policy.firstVoicedAt)
	}
	if got := r.u.at(0); got < 9*time.Second {
		t.Fatalf("policy clock at the trimmed clip start = %v, want the pre-roll offset", got)
	}
}

// A packet that straddles the VAD frame where speech starts must appear once in
// the clip: the pre-roll splice may not duplicate the post-frame bytes.
func TestEndpointRunSpliceCopiesOnce(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(true)
	quiet := make([]byte, 600*time.Millisecond.Milliseconds()*sampleRate*2/1000)
	voiced := make([]byte, len(quiet))
	for i := 0; i < len(voiced); i += 2 {
		voiced[i], voiced[i+1] = 0xE8, 0x03
	}
	signal := slices.Concat(quiet, voiced)
	const chunk = 8192 // not a multiple of endpointBytes
	at := time.Unix(0, 0)
	for pos := 0; pos < len(signal); pos += chunk {
		feedEndpoint(r, signal[pos:min(pos+chunk, len(signal))], at)
		at = at.Add(endpointFrame)
	}
	want := slices.Concat(quiet[len(quiet)-listenPreRollBytes:], voiced)
	if !bytes.Equal(r.u.clip(), want) {
		t.Fatalf("clip = %d bytes, want %d (duplicated or dropped splice)", len(r.u.clip()), len(want))
	}
}

// Speech starts at the first gated packet, not at commit time.
func TestEndpointRunSpeechAtFirstVoicedPacket(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(true)
	base := time.Unix(1000, 0)
	at := base
	feedEndpointFrames(r, silentEndpointFrame, 3, &at)
	firstVoiced := base.Add(9 * time.Second)
	at = firstVoiced
	feedEndpointFrames(r, voicedEndpointFrame, 2, &at)
	if !r.speechAt.Equal(firstVoiced) {
		t.Fatalf("speechAt = %v, want first voiced packet %v", r.speechAt, firstVoiced)
	}
	feedEndpointFrames(r, voicedEndpointFrame, 2, &at)
	if !r.speechAt.Equal(firstVoiced) {
		t.Fatalf("speechAt moved to %v; the name rule must judge the speech start", r.speechAt)
	}
}

// The follow-up grace boundary is judged against speechAt, so a commit that
// lands after the grace does not retroactively require the name.
func TestEndpointRunNameGraceBoundary(t *testing.T) {
	t.Parallel()
	base := time.Unix(2000, 0)
	h := &terminalHub{}
	d := &terminalSession{}
	d.nameFreeUntil = base.Add(10 * time.Second)
	if h.nameRequired(d, base.Add(9999*time.Millisecond)) {
		t.Fatal("speech just inside the grace required the name")
	}
	if !h.nameRequired(d, base.Add(10001*time.Millisecond)) {
		t.Fatal("speech just after the grace did not require the name")
	}
}

// The silence commit needs both the quiet window and Smart Turn agreement.
func TestEndpointRunSilenceCommit(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "Tell me a story", false)
	feedEndpointFrames(r, silentEndpointFrame, 20, &at) // 400 ms quiet
	r.smartProb = 0.5
	if got := r.decision(); got != decisionContinue {
		t.Fatalf("without Smart Turn agreement: %v, want continue", got)
	}
	r.smartProb = smartTurnThreshold
	if got := r.decision(); got != decisionCommitQuiet {
		t.Fatalf("at the quiet window with Smart Turn: %v, want commit quiet", got)
	}
	if got := r.u.policy.decide(r.u.at(len(r.u.pcm)), endpointCap, r.limitSpan, r.smartProb, false); got != endpointCommit {
		t.Fatalf("legacy decide = %v, want commit", got)
	}
}

// Silence past the cap commits whatever the transcript says.
func TestEndpointRunCapCommit(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "Where is the dragon?", false)
	feedEndpointFrames(r, silentEndpointFrame, 60, &at) // 1200 ms
	if got := r.decision(); got != decisionCommitCap {
		t.Fatalf("at the cap: %v, want commit cap", got)
	}
}

// Speech with no usable transcript commits as "didn't catch that" rather than
// extending the capture forever.
func TestEndpointRunNoWordsCommit(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	if r.u.policy.hasWords {
		t.Fatal("test needs a run without a transcript")
	}
	feedEndpointFrames(r, silentEndpointFrame, 60, &at)
	if got := r.decision(); got != decisionCommitNoWords {
		t.Fatalf("no words at the cap: %v, want commit no-words", got)
	}
}

// Continuous speech becomes a turn at the monologue cap.
func TestEndpointRunMonologueCommit(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1005, &at) // just over 20 s
	if got := r.decision(); got != decisionCommitMonologue {
		t.Fatalf("20 s monologue: %v, want commit monologue", got)
	}
}

// A tap-to-talk window that never heard speech discards at the idle deadline.
func TestEndpointRunDiscardIdle(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, silentEndpointFrame, int(endpointIdle/endpointFrame), &at)
	if got := r.decision(); got != decisionDiscardIdle {
		t.Fatalf("idle window: %v, want discard idle", got)
	}
	if got := r.u.policy.decide(r.u.at(len(r.u.pcm)), endpointCap, r.limitSpan, 0, false); got != endpointDiscard {
		t.Fatalf("legacy decide = %v, want discard", got)
	}
}

// Noise that never crossed the speech gate is discarded at the window limit.
func TestEndpointRunDiscardNoise(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	r.limitSpan = time.Second
	r.u.limit = utteranceBytes(r.limitSpan)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, silentEndpointFrame, 50, &at)
	if r.u.policy.speechFrames != 0 {
		t.Fatal("test needs a window with no gated speech")
	}
	if got := r.decision(); got != decisionDiscardNoise {
		t.Fatalf("noise at the limit: %v, want discard noise", got)
	}
}

// The commit may reuse a probe transcript only when no gated audio followed it.
func TestEndpointRunClosingPauseGuard(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	r.cached = pauseTranscript{audioLen: 320, text: "hello world"}
	r.lastGateOpenLen = 320
	if !r.pauseCoversSpeech() {
		t.Fatal("probe covering all gated speech was not reusable")
	}
	r.lastGateOpenLen = 322
	if r.pauseCoversSpeech() {
		t.Fatal("probe that missed later gated audio was reused")
	}
	r.lastGateOpenLen = 320
	r.cached.windowed = true
	if r.pauseCoversSpeech() {
		t.Fatal("windowed probe was reused")
	}
	r.cached = pauseTranscript{audioLen: 320, text: ""}
	if r.pauseCoversSpeech() {
		t.Fatal("empty probe text was reused")
	}
}

// A terminal sentence closes the thought; the same words without punctuation
// keep waiting for the pause or the cap.
func TestEndpointRunTerminalPunctuation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text string
		want decision
	}{
		{"I like that.", decisionCommitQuiet},
		{"I like that?", decisionCommitQuiet},
		{"I like that", decisionContinue},
		{"Can you put his", decisionContinue},
	} {
		r := testEndpointRun(false)
		at := time.Unix(0, 0)
		feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
		r.u.policy.observeProgress(r.u.policy.lastVoicedAt, tc.text, false)
		feedEndpointFrames(r, silentEndpointFrame, 20, &at)
		r.smartProb = smartTurnThreshold
		if got := r.decision(); got != tc.want {
			t.Fatalf("%q: %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestEndpointDecisionsMapToLegacy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d    decision
		want endpointDecision
	}{
		{decisionContinue, endpointWait},
		{decisionCommitQuiet, endpointCommit},
		{decisionCommitCap, endpointCommit},
		{decisionCommitMonologue, endpointCommit},
		{decisionCommitNoWords, endpointCommit},
		{decisionDiscardIdle, endpointDiscard},
		{decisionDiscardNoise, endpointDiscard},
	} {
		if got := tc.d.discards(); got != (tc.want == endpointDiscard) {
			t.Fatalf("%v.discards() = %v, want %v", tc.d, got, tc.want == endpointDiscard)
		}
		if got := tc.d.stops(); got != (tc.want != endpointWait) {
			t.Fatalf("%v.stops() = %v, want %v", tc.d, got, tc.want != endpointWait)
		}
	}
}

// A short unfinished fragment with a very low Smart Turn probability waits
// past the normal cap, and the child continuing mid-wait resumes cleanly.
func TestEndpointRunHesitationContinuesWhenKidResumes(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "can you put his", false)
	r.smartProb = 0.05
	feedEndpointFrames(r, silentEndpointFrame, 60, &at) // 1200 ms, past the cap
	if got := r.decision(); got != decisionContinue {
		t.Fatalf("hesitation at the cap: %v, want continue", got)
	}
	feedEndpointFrames(r, voicedEndpointFrame, 20, &at) // resumes at about 1.6 s
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "can you put his shoes on please.", false)
	r.smartProb = 0.92
	feedEndpointFrames(r, silentEndpointFrame, 20, &at) // 400 ms after the resume
	if got := r.decision(); got != decisionCommitQuiet {
		t.Fatalf("resumed complete thought: %v, want commit quiet", got)
	}
}

// If the child truly stopped, the extra second ends and the turn commits.
func TestEndpointRunHesitationCommitsAtExtendedCap(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "can you put his", false)
	r.smartProb = 0.05
	feedEndpointFrames(r, silentEndpointFrame, 60, &at)
	if got := r.decision(); got != decisionContinue {
		t.Fatalf("before the hesitation cap: %v, want continue", got)
	}
	feedEndpointFrames(r, silentEndpointFrame, 55, &at) // past 2.2 s total
	if got := r.decision(); got != decisionCommitCap {
		t.Fatalf("at the hesitation cap: %v, want commit cap", got)
	}
}

// A normal complete thought keeps the 400 ms quiet commit.
func TestEndpointRunNormalThoughtStillCommitsAtQuiet(t *testing.T) {
	t.Parallel()
	r := testEndpointRun(false)
	at := time.Unix(0, 0)
	feedEndpointFrames(r, voicedEndpointFrame, 1, &at)
	r.u.policy.observeProgress(r.u.policy.lastVoicedAt, "What time is it", false)
	r.smartProb = 0.92
	feedEndpointFrames(r, silentEndpointFrame, 20, &at) // 400 ms quiet
	if got := r.decision(); got != decisionCommitQuiet {
		t.Fatalf("normal thought: %v, want commit quiet", got)
	}
}
