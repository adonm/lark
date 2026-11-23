package main

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type diagMetric int

const (
	diagSTT diagMetric = iota
	diagLLMFirst
	diagLLMTotal
	diagTTSFirst
	diagE2E
	diagRender
	diagFrameBytes
	diagRawFrameBytes
	diagGate
	diagFirstSentence
	diagTTSQueue
	diagTTSInfer
	diagFirstPCM
	diagEndpoint
	diagPromptPrep
	diagPromptSubmit
	diagVADQueue
	diagSTTQueue
	diagSTTRun
	diagResample
	diagSpecFirst
	diagMetricCount
)

var diagMetricNames = [diagMetricCount]string{
	"stt_ms", "llm_first_ms", "llm_total_ms", "tts_first_ms", "e2e_ms",
	"render_ms", "frame_bytes", "frame_raw_bytes",
	"gate_ms", "first_sentence_ms", "tts_queue_ms", "tts_infer_ms", "first_pcm_ms",
	"speech_end_to_commit_ms",
	"prompt_prep_ms", "prompt_submit_ms",
	"vad_queue_ms", "stt_queue_ms", "stt_run_ms", "resample_ms",
	"spec_first_ms",
}

const diagInterval = 2 * time.Second

type diagRing[T any] struct {
	items []T
	next  int
}

func (r *diagRing[T]) add(value T, limit int) {
	if len(r.items) < limit {
		r.items = append(r.items, value)
	} else {
		r.items[r.next] = value
	}
	r.next = (r.next + 1) % limit
}

func (r diagRing[T]) each(visit func(T)) {
	for i := range r.items {
		visit(r.items[(r.next+i)%len(r.items)])
	}
}

func (r diagRing[T]) last() (out T) {
	if n := len(r.items); n > 0 {
		return r.items[(r.next+n-1)%n]
	}
	return out
}

type diagSummary struct {
	Last    *float64  `json:"last"`
	P50     *float64  `json:"p50"`
	P95     *float64  `json:"p95"`
	Count   int       `json:"count"`
	Samples []float64 `json:"samples"`
}

func summarize(r diagRing[float64]) diagSummary {
	s := diagSummary{Count: len(r.items), Samples: make([]float64, 0, len(r.items))}
	r.each(func(v float64) { s.Samples = append(s.Samples, v) })
	if s.Count != 0 {
		s.Last = new(r.last())
		ordered := slices.Clone(s.Samples)
		slices.Sort(ordered)
		// Nearest-rank percentiles; the window contains at most 32 samples.
		s.P50 = &ordered[(s.Count*50+99)/100-1]
		s.P95 = &ordered[(s.Count*95+99)/100-1]
	}
	return s
}

type diagTurn struct {
	TS            int64    `json:"ts_ms"`
	Terminal      string   `json:"terminal"`
	Character     string   `json:"character"`
	Source        string   `json:"source"`
	STT           *float64 `json:"stt_ms"`
	First         *float64 `json:"llm_first_ms"`
	Total         *float64 `json:"llm_total_ms"`
	TTS           *float64 `json:"tts_first_ms"`
	E2E           *float64 `json:"e2e_ms"`
	Gate          *float64 `json:"gate_ms"`
	FirstSentence *float64 `json:"first_sentence_ms"`
	TTSQueue      *float64 `json:"tts_queue_ms"`
	TTSInfer      *float64 `json:"tts_infer_ms"`
	Render        *float64 `json:"render_ms"`
	FirstPCM      *float64 `json:"first_pcm_ms"`
	Endpoint      *float64 `json:"speech_end_to_commit_ms"`
	PromptPrep    *float64 `json:"prompt_prep_ms"`
	PromptSubmit  *float64 `json:"prompt_submit_ms"`
	VADQueue      *float64 `json:"vad_queue_ms"`
	STTQueue      *float64 `json:"stt_queue_ms"`
	STTRun        *float64 `json:"stt_run_ms"`
	Resample      *float64 `json:"resample_ms"`
	Error         bool     `json:"error,omitempty"`
}

type diagFrame struct {
	MS       float64 `json:"render_ms"`
	Bytes    int     `json:"frame_bytes"`
	RawBytes int     `json:"frame_raw_bytes"`
	Codec    string  `json:"frame_codec"`
}

type diagnostics struct {
	mu      sync.Mutex
	sig     condSignal
	metrics [diagMetricCount]diagRing[float64]
	turns   diagRing[*diagTurn]
	system  diagSystem
	refresh diagSchemaRefresh
	spec    diagSpec
}

// Speculation counters are cumulative because the bench and QA suites need an
// abort rate even when debug logging is off.
type diagSpec struct {
	started, hit, abortWords, abortSpeech, abortMismatch, abortOther atomic.Int64
	overlapMS                                                        atomic.Int64 // commits: summed spec lead time
}

type diagSpecReport struct {
	Started       int64   `json:"started"`
	Hit           int64   `json:"hit"`
	AbortWords    int64   `json:"abort_words"`
	AbortSpeech   int64   `json:"abort_speech"`
	AbortMismatch int64   `json:"abort_mismatch"`
	AbortOther    int64   `json:"abort_other"`
	OverlapAvgMS  float64 `json:"overlap_avg_ms"`
}

type turnTrace struct {
	d      *diagnostics
	record *diagTurn
	start  time.Time
}

type traceKey struct{}

func (b *larkServer) traceTurn(ctx context.Context, terminal, character, source string, at time.Time) context.Context {
	t := &turnTrace{d: &b.diag, start: at, record: &diagTurn{TS: at.UnixMilli(), Terminal: terminal, Character: character, Source: source}}
	b.diag.mu.Lock()
	b.diag.turns.add(t.record, 20)
	b.diag.mu.Unlock()
	b.diag.sig.notify()
	return context.WithValue(ctx, traceKey{}, t)
}

func traceFrom(ctx context.Context) *turnTrace {
	t, _ := ctx.Value(traceKey{}).(*turnTrace)
	return t
}

func milliseconds(d time.Duration) float64 {
	return math.Round(max(0, float64(d)/float64(time.Millisecond))*100) / 100
}

func (t *turnTrace) fail() {
	if t == nil {
		return
	}
	t.d.mu.Lock()
	t.record.Error = true
	t.d.mu.Unlock()
	t.d.sig.notify()
}

func (t *turnTrace) measure(metric diagMetric, elapsed time.Duration) {
	if t == nil {
		return
	}
	value := milliseconds(elapsed)
	t.d.mu.Lock()
	field := [diagMetricCount]**float64{
		diagSTT: &t.record.STT, diagLLMFirst: &t.record.First, diagLLMTotal: &t.record.Total,
		diagTTSFirst: &t.record.TTS, diagE2E: &t.record.E2E, diagRender: &t.record.Render,
		diagGate: &t.record.Gate, diagFirstSentence: &t.record.FirstSentence,
		diagTTSQueue: &t.record.TTSQueue, diagTTSInfer: &t.record.TTSInfer, diagFirstPCM: &t.record.FirstPCM,
		diagEndpoint:   &t.record.Endpoint,
		diagPromptPrep: &t.record.PromptPrep, diagPromptSubmit: &t.record.PromptSubmit,
		diagVADQueue: &t.record.VADQueue, diagSTTQueue: &t.record.STTQueue,
		diagSTTRun: &t.record.STTRun, diagResample: &t.record.Resample,
	}[metric]
	if field != nil && *field == nil {
		*field = &value
		if metric != diagRender { // The aggregate already includes native frame samples.
			t.d.metrics[metric].add(value, 32)
		}
	}
	t.d.mu.Unlock()
	t.d.sig.notify()
}

// specFirst records time-to-first-sentence for a speculative call. It is not
// part of a turn trace because the call starts before the turn commits.
func (d *diagnostics) specFirst(elapsed time.Duration) {
	d.mu.Lock()
	d.metrics[diagSpecFirst].add(milliseconds(elapsed), 32)
	d.mu.Unlock()
	d.sig.notify()
}

func (t *turnTrace) audioEnqueued(at time.Time) {
	if t != nil {
		t.firstPCM(at)
		if t.record.Source == "voice" {
			t.measure(diagE2E, at.Sub(t.start))
		}
	}
}

func (t *turnTrace) firstPCM(at time.Time) {
	if t != nil {
		t.measure(diagFirstPCM, at.Sub(t.start))
	}
}

func (b *larkServer) transcribe(ctx context.Context, pcm []byte) (string, error) {
	start := time.Now()
	sttCtx, timing := withSTTTiming(ctx)
	text, err := b.decodeClip(sttCtx, pcm)
	trace := traceFrom(ctx)
	trace.measure(diagSTT, time.Since(start))
	if timing.run > 0 || timing.queue > 0 {
		trace.measure(diagSTTQueue, timing.queue)
		trace.measure(diagSTTRun, timing.run)
	}
	return text, err
}

func (h *terminalHub) trackFrame(d *terminalSession, elapsed time.Duration, size, raw int, codec string) {
	d.frame = &diagFrame{milliseconds(elapsed), size, raw, codec}
	if codec == "svg" {
		return
	}
	stats := &h.b.diag
	stats.mu.Lock()
	stats.metrics[diagRender].add(d.frame.MS, 32)
	stats.metrics[diagFrameBytes].add(float64(size), 32)
	stats.metrics[diagRawFrameBytes].add(float64(raw), 32)
	stats.mu.Unlock()
}

type diagProcess struct {
	CPU *float64 `json:"cpu_pct"`
	RSS *uint64  `json:"rss_bytes"`
}

type diagBridge struct {
	diagProcess
	HeapAlloc  uint64     `json:"heap_alloc_bytes"`
	HeapInuse  uint64     `json:"heap_inuse_bytes"`
	Goroutine  int        `json:"goroutines"`
	Uptime     int64      `json:"uptime_s"`
	DB         int64      `json:"db_bytes"`
	LastBackup *time.Time `json:"last_backup"`
}

type diagTTS struct {
	Engine string `json:"engine"`
	Ready  bool   `json:"ready"`
}

type diagHost struct {
	CPU   *float64 `json:"cpu_pct"`
	Total *uint64  `json:"mem_total_bytes"`
	Used  *uint64  `json:"mem_used_bytes"`
	Load  *float64 `json:"load_1m"`
}

type diagServices struct {
	STT        bool   `json:"stt"`
	STTEngine  string `json:"stt_engine"`
	OpenCode   bool   `json:"opencode"`
	TTS        bool   `json:"tts"`
	OpenTunnel bool   `json:"opentunnel"`
}

type diagSystem struct {
	Bridge   diagBridge   `json:"bridge"`
	TTS      diagTTS      `json:"tts"`
	Host     diagHost     `json:"host"`
	Services diagServices `json:"services"`
}

type diagReport struct {
	Generated   time.Time              `json:"generated_at"`
	Latency     map[string]diagSummary `json:"latency"`
	Turns       []diagTurn             `json:"recent_turns"`
	Terminals   []diagTerminal         `json:"terminals"`
	SVGCache    diagCache              `json:"svg_cache"`
	Refresh     diagSchemaRefresh      `json:"mcp_contract_refresh"`
	Speculation diagSpecReport         `json:"speculation"`
	Library     object                 `json:"library"`
	diagSystem
}

type diagCache struct {
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

func (b *larkServer) diagnosticReport() diagReport {
	out := diagReport{Generated: time.Now().UTC(), Latency: make(map[string]diagSummary), Turns: []diagTurn{}, Terminals: []diagTerminal{}}
	b.diag.mu.Lock()
	out.diagSystem = b.diag.system
	out.Refresh = b.diag.refresh
	out.Speculation = diagSpecReport{
		Started:       b.diag.spec.started.Load(),
		Hit:           b.diag.spec.hit.Load(),
		AbortWords:    b.diag.spec.abortWords.Load(),
		AbortSpeech:   b.diag.spec.abortSpeech.Load(),
		AbortMismatch: b.diag.spec.abortMismatch.Load(),
		AbortOther:    b.diag.spec.abortOther.Load(),
	}
	if hits := b.diag.spec.hit.Load(); hits > 0 {
		out.Speculation.OverlapAvgMS = float64(b.diag.spec.overlapMS.Load()) / float64(hits)
	}
	if out.Refresh.State == "" {
		out.Refresh.State = "not_started"
	}
	for i, name := range diagMetricNames {
		out.Latency[name] = summarize(b.diag.metrics[i])
	}
	b.diag.turns.each(func(t *diagTurn) { out.Turns = append(out.Turns, *t) })
	b.diag.mu.Unlock()
	if b.db != nil {
		if at := b.db.lastBackup.Load(); at != 0 {
			out.Bridge.LastBackup = new(time.Unix(at, 0).UTC())
		}
	}
	slices.Reverse(out.Turns)
	out.Library = b.libraryReport()
	if b.hub != nil {
		out.Terminals = b.hub.diagTerminals()
	}
	return out
}

type diagTerminal struct {
	ID           string     `json:"terminal_id"`
	CompanionID  string     `json:"companion_id,omitempty"`
	Character    string     `json:"name"`
	Kind         string     `json:"kind"`
	Board        string     `json:"board,omitempty"`
	Firmware     string     `json:"firmware,omitempty"`
	OTABundle    bool       `json:"ota_bundle"`
	NoteEndpoint string     `json:"note_endpoint,omitempty"`
	Mode         string     `json:"mode"`
	RTT          *float64   `json:"rtt_ms"`
	Frame        *diagFrame `json:"last_frame"`
	Ambient      *float64   `json:"ambient_rms"`
	Auto         bool       `json:"auto_volume"`
	terminalTelemetry
}

func (d *terminalSession) diagTerminal() diagTerminal {
	s := d.telemetry
	s.Volume = new(d.vol)
	out := diagTerminal{ID: d.id, CompanionID: d.companionID, Character: d.companion.Name, Kind: d.kind, Board: d.board,
		Firmware: d.firmware, Mode: d.pose.mood, RTT: d.client.pingRTT(),
		Frame: d.frame, Auto: d.autoVol, terminalTelemetry: s}
	if d.hasAmbient {
		out.Ambient = new(d.ambientEMA)
	}
	return out
}

func (h *terminalHub) diagTerminals() []diagTerminal {
	terms, _ := h.b.terminalSnapshot()
	roundOTA, boxOTA := fileBytes(h.b.ota.path) > 0, fileBytes(h.b.box3OTA.path) > 0
	outMap := make(map[string]diagTerminal, len(terms))
	for id, t := range terms {
		name := ""
		if t.CompanionID != "" {
			name = h.b.companionFor(t.CompanionID).Name
		}
		outMap[id] = diagTerminal{ID: t.ID, CompanionID: t.CompanionID, Character: name, Kind: t.Kind, Mode: "offline"}
	}
	h.mu.Lock()
	for id, d := range h.terminals {
		if h.active(d) {
			term := d.diagTerminal()
			term.NoteEndpoint = "client"
			if h.bridgeEndpoint(d) {
				term.NoteEndpoint = "bridge"
			}
			term.OTABundle = term.Board == "esp-box-3" && boxOTA || term.Board == "esp-vocat" && roundOTA
			outMap[id] = term
		}
	}
	h.mu.Unlock()
	out := make([]diagTerminal, 0, len(outMap))
	for _, d := range outMap {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b diagTerminal) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type cpuReading struct{ user, total uint64 }
type procReading struct {
	pid     int
	ticks   uint64
	started uint64
}

func fileText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func fileBytes(path string) int64 {
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return info.Size()
	}
	return 0
}

func linuxClockTicks() float64 {
	vec, _ := os.ReadFile("/proc/self/auxv")
	word := strconv.IntSize / 8
	for i := 0; i+2*word <= len(vec); i += 2 * word {
		read := func(b []byte) uint64 {
			if word == 4 {
				return uint64(binary.NativeEndian.Uint32(b))
			}
			return binary.NativeEndian.Uint64(b)
		}
		if read(vec[i:]) == 17 { // AT_CLKTCK
			if n := read(vec[i+word:]); n != 0 {
				return float64(n)
			}
		}
	}
	return 100 // Linux USER_HZ on supported architectures.
}

func parseProcStat(raw string, pid int) procReading {
	// comm is parenthesized and may contain spaces or closing parentheses.
	i := strings.LastIndexByte(raw, ')')
	if i < 0 {
		return procReading{}
	}
	fields := strings.Fields(raw[i+1:])
	if len(fields) < 20 {
		return procReading{}
	}
	user, e1 := strconv.ParseUint(fields[11], 10, 64)
	system, e2 := strconv.ParseUint(fields[12], 10, 64)
	started, e3 := strconv.ParseUint(fields[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return procReading{}
	}
	return procReading{pid, user + system, started}
}

func sampleProcess(path string, pid int, prev *procReading, seconds, hz float64) diagProcess {
	var out diagProcess
	next := parseProcStat(fileText(path+"/stat"), pid)
	if next.pid == prev.pid && next.pid != 0 && next.started == prev.started && next.ticks >= prev.ticks && seconds > 0 {
		out.CPU = new(100 * float64(next.ticks-prev.ticks) / (hz * seconds))
	}
	*prev = next
	if fields := strings.Fields(fileText(path + "/statm")); len(fields) > 1 {
		if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			out.RSS = new(pages * uint64(os.Getpagesize()))
		}
	}
	return out
}

func parseHostCPU(raw string) (out cpuReading) {
	fields := strings.Fields(strings.SplitN(raw, "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return out
	}
	for i, s := range fields[1:min(len(fields), 9)] {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuReading{}
		}
		out.total += n
		if i != 3 && i != 4 { // idle and iowait; guest time is already in user/nice.
			out.user += n
		}
	}
	return out
}

func sampleHost(prev *cpuReading) (out diagHost) {
	next := parseHostCPU(fileText("/proc/stat"))
	if prev.total != 0 && next.total > prev.total && next.user >= prev.user {
		out.CPU = new(min(100., 100*float64(next.user-prev.user)/float64(next.total-prev.total)))
	}
	*prev = next
	var available *uint64
	for line := range strings.SplitSeq(fileText("/proc/meminfo"), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		kb, _, _ := strings.Cut(strings.TrimSpace(value), " ")
		if n, err := strconv.ParseUint(kb, 10, 64); err == nil {
			if key == "MemTotal" {
				out.Total = new(n * 1024)
			} else {
				available = new(n * 1024)
			}
		}
	}
	if out.Total != nil && available != nil && *out.Total >= *available {
		out.Used = new(*out.Total - *available)
	}
	if one, _, _ := strings.Cut(fileText("/proc/loadavg"), " "); one != "" {
		if n, err := strconv.ParseFloat(one, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			out.Load = &n
		}
	}
	return out
}

func (b *larkServer) sampleDiagnostics(start time.Time, path string) func() {
	ctx, cancel := context.WithCancel(b.root)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(diagInterval)
		defer ticker.Stop()
		hz := linuxClockTicks()
		var self procReading
		var cpu cpuReading
		last := time.Now()
		for ctx.Err() == nil {
			now := time.Now()
			seconds := now.Sub(last).Seconds()
			last = now
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			report := diagSystem{Bridge: diagBridge{
				diagProcess: sampleProcess("/proc/self", os.Getpid(), &self, seconds, hz),
				HeapAlloc:   stats.HeapAlloc, HeapInuse: stats.HeapInuse, Goroutine: runtime.NumGoroutine(),
				Uptime: int64(now.Sub(start) / time.Second), DB: fileBytes(path) + fileBytes(path+"-wal"),
			}, Host: sampleHost(&cpu)}
			if b.worker != nil {
				report.TTS = diagTTS{kokoroEngine, b.worker.ready.Load()}
				report.Services.TTS = report.TTS.Ready
			}
			if b.oc != nil && b.oc.config != nil {
				report.Services.OpenCode = b.oc.reachable(ctx, "/api/info")
			}
			report.Services.STT = b.sttFunc() != nil
			report.Services.STTEngine = sttEngine
			report.Services.OpenTunnel = b.remote != nil && b.remote.available()
			b.diag.mu.Lock()
			b.diag.system = report
			b.diag.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
