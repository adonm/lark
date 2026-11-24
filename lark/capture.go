package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	captureTranscribeTimeout = 90 * time.Second
	// Debug captures are a local audit trail, not an archive: keep two weeks
	// and at most 1 GiB, pruning oldest first on start and after each save.
	captureRetention = 14 * 24 * time.Hour
	captureMaxBytes  = int64(1) << 30
	captureListMax   = 200
	captureReadMax   = 64 << 20 // One capture's WAV must fit comfortably in memory.
	// The remote page shows a finished capture briefly, then returns to idle.
	captureDoneWindow = 20 * time.Second
	// A capture run gets its own window plus this slack before timing out.
	captureTimeoutSlack = 30 * time.Second
	// 16 kHz mono s16le, the width capture WAVs and transcripts share.
	captureBytesPerSecond = 2 * sampleRate
	captureWAVHeader      = 44 // Canonical PCM WAV header size.
)

// debugCaptureDir is local to the host, mode 0700, and never uploaded to a
// third party. Captures are served only to the holder of a scoped remote link.
func debugCaptureDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "lark", "debug")
}

var captureNameRE = regexp.MustCompile(`^capture-(.+)-(\d{8}-\d{6})(?:-\d+)?\.wav$`)

func captureCompanion(name string) (string, bool) {
	m := captureNameRE.FindStringSubmatch(name)
	if m == nil || filepath.Base(name) != name {
		return "", false
	}
	return m[1], true
}

// captureID is the remote-facing name without the .wav extension.
func captureID(name string) string { return strings.TrimSuffix(name, ".wav") }

// capturePath accepts only a listed capture ID for this companion, so the
// scoped remote routes cannot walk outside the debug directory.
func capturePath(companionID, id string) (string, bool) {
	name := id + ".wav"
	owner, ok := captureCompanion(name)
	if !ok || owner != companionID {
		return "", false
	}
	return filepath.Join(debugCaptureDir(), name), true
}

func captureTranscriptPath(wavPath string) string {
	return strings.TrimSuffix(wavPath, ".wav") + ".txt"
}

type captureGroup struct {
	base string
	mod  time.Time
	size int64
}

// pruneCaptures enforces the local retention limit. It returns the bytes
// removed; failures are reported so the caller can log them without blocking
// a capture.
func pruneCaptures(dir string, maxAge time.Duration, maxBytes int64) (int64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	groups := map[string]*captureGroup{}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".wav"), ".txt")
		if _, ok := captureCompanion(base + ".wav"); !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		group := groups[base]
		if group == nil {
			group = &captureGroup{base: base}
			groups[base] = group
		}
		group.size += info.Size()
		if strings.HasSuffix(entry.Name(), ".wav") || group.mod.IsZero() {
			group.mod = info.ModTime()
		}
	}
	ordered := make([]*captureGroup, 0, len(groups))
	var total int64
	for _, group := range groups {
		ordered = append(ordered, group)
		total += group.size
	}
	slices.SortFunc(ordered, func(a, b *captureGroup) int { return a.mod.Compare(b.mod) })
	var removed int64
	for _, group := range ordered {
		if total <= maxBytes && time.Since(group.mod) <= maxAge {
			break
		}
		for _, path := range []string{filepath.Join(dir, group.base+".wav"), filepath.Join(dir, group.base+".txt")} {
			if info, err := os.Stat(path); err == nil {
				if os.Remove(path) == nil {
					total -= info.Size()
					removed += info.Size()
				}
			}
		}
	}
	return removed, nil
}

func deleteCompanionCaptures(dir, companionID string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".wav"), ".txt")
		if owner, ok := captureCompanion(base + ".wav"); !ok || owner != companionID {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writePrivateFile writes through a sibling temp file so an interrupted save
// never leaves a truncated WAV or transcript behind.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".capture-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func writeCaptureWAV(path string, pcm []byte) error {
	hdr := wavHeader(len(pcm))
	out := make([]byte, len(hdr)+len(pcm))
	copy(out, hdr[:])
	copy(out[len(hdr):], pcm)
	return writePrivateFile(path, out)
}

// captureRun is one capture window. Both `lark capture` (through
// /api/capture) and the scoped remote routes start here, so the listening
// path, filename, permissions, and error handling cannot drift apart.
type captureRun struct {
	name        string
	companionID string
	seconds     int
	started     time.Time
	result      <-chan captureResult
}

func (b *larkServer) beginCapture(companionID string, seconds int) (*captureRun, error) {
	if _, ok := b.companionByID(companionID); !ok {
		return nil, problem(404, "Companion not found")
	}
	if removed, err := pruneCaptures(debugCaptureDir(), captureRetention, captureMaxBytes); err != nil {
		log.Printf("Lark could not prune debug captures: %v", err)
	} else if removed > 0 {
		log.Printf("Lark pruned %d bytes of debug captures", removed)
	}
	if b.hub == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	span := time.Duration(seconds) * time.Second
	now := time.Now()
	b.hub.mu.Lock()
	d := b.hub.voiceOwner(companionID)
	if d == nil {
		b.hub.mu.Unlock()
		return nil, problem(409, "No listening device available for this companion")
	}
	if d.recording.active() {
		b.hub.mu.Unlock()
		return nil, problem(409, "A capture is already running for this companion")
	}
	if d.busy || d.queuedVoice != nil || d.ttsStarted || d.awaitAck {
		b.hub.mu.Unlock()
		return nil, problem(409, "The companion is busy; try again when the turn ends")
	}
	if !b.hub.startCaptureWindow(d, now, span) {
		b.hub.mu.Unlock()
		return nil, problem(409, "This device cannot stream microphone audio")
	}
	c := d.recording.endpoint
	b.hub.mu.Unlock()
	if c == nil {
		return nil, problem(409, "No listening device available for this companion")
	}
	if debugEndpoint {
		log.Printf("capture start term=%s span=%v ctxErr=%v", d.id, span, d.client.ctx.Err())
	}
	return &captureRun{name: uniqueCaptureName(companionID, now), companionID: companionID,
		seconds: seconds, started: now, result: c.result}, nil
}

func uniqueCaptureName(companionID string, now time.Time) string {
	base := fmt.Sprintf("capture-%s-%s", companionID, now.Format("20060102-150405"))
	for i := 1; ; i++ {
		name := base + ".wav"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.wav", base, i)
		}
		if _, err := os.Stat(filepath.Join(debugCaptureDir(), name)); err != nil {
			return name
		}
	}
}

func (b *larkServer) waitCapture(ctx context.Context, run *captureRun) (captureResult, error) {
	select {
	case result, ok := <-run.result:
		if !ok {
			return captureResult{}, problem(409, "No microphone audio arrived")
		}
		if debugEndpoint {
			log.Printf("capture result name=%s bytes=%d err=%v", run.name, len(result.pcm), result.err)
		}
		return result, nil
	case <-ctx.Done():
		return captureResult{}, problem(504, "Capture timed out")
	}
}

func (b *larkServer) saveCapture(run *captureRun, result captureResult) (string, error) {
	if len(result.pcm) == 0 {
		return "", problem(409, "No microphone audio arrived")
	}
	path := filepath.Join(debugCaptureDir(), run.name)
	if err := writeCaptureWAV(path, result.pcm); err != nil {
		return "", problem(500, "Cannot write capture: "+err.Error())
	}
	if removed, err := pruneCaptures(debugCaptureDir(), captureRetention, captureMaxBytes); err != nil {
		log.Printf("Lark could not prune debug captures: %v", err)
	} else if removed > 0 {
		log.Printf("Lark pruned %d bytes of debug captures", removed)
	}
	return path, nil
}

func (b *larkServer) captureAPI(r *http.Request) (any, error) {
	var req captureRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	if req.CompanionID == "" {
		return nil, problem(400, "companion_id is required")
	}
	run, err := b.startCapture(req.CompanionID, req, captureDefaultSeconds)
	if err != nil {
		return nil, err
	}
	result, err := b.waitCapture(r.Context(), run)
	if err != nil {
		return nil, err
	}
	path, err := b.saveCapture(run, result)
	if err != nil {
		return nil, err
	}
	return object{
		"path": path, "companion_id": run.companionID, "seconds": run.seconds,
		"audio_ms": len(result.pcm) * 1000 / captureBytesPerSecond, "bytes": len(result.pcm),
	}, nil
}

type captureJob struct {
	name        string
	companionID string
	seconds     int
	started     time.Time
}

type captureOutcome struct {
	name string
	ok   bool
	err  string
	at   time.Time
}

// captureState is the small live/seen state behind the remote page. The hub
// owns the microphone; this only tracks what the page needs for SSE pushes.
type captureState struct {
	mu           sync.Mutex
	active       map[string]*captureJob
	done         map[string]captureOutcome
	transcribing map[string]bool
}

func (s *captureState) start(run *captureRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		s.active = make(map[string]*captureJob)
	}
	if s.done != nil {
		delete(s.done, run.companionID)
	}
	s.active[run.companionID] = &captureJob{name: captureID(run.name), companionID: run.companionID,
		seconds: run.seconds, started: run.started}
}

func (s *captureState) finish(run *captureRun, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := captureID(run.name)
	if job := s.active[run.companionID]; job != nil && job.name == id {
		delete(s.active, run.companionID)
	}
	if s.done == nil {
		s.done = make(map[string]captureOutcome)
	}
	out := captureOutcome{name: id, ok: err == nil, at: time.Now()}
	if err != nil {
		out.err = err.Error()
	}
	s.done[run.companionID] = out
}

func (s *captureState) status(companionID string) object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job := s.active[companionID]; job != nil {
		remaining := int((time.Until(job.started.Add(time.Duration(job.seconds)*time.Second)) + time.Second - 1) / time.Second)
		remaining = max(0, min(job.seconds, remaining))
		return object{"state": "recording", "id": job.name, "name": job.name, "seconds": job.seconds,
			"remaining": remaining, "started_at": job.started.UTC().Format(time.RFC3339)}
	}
	if out := s.done[companionID]; out.name != "" && time.Since(out.at) < captureDoneWindow {
		item := object{"state": "done", "id": out.name, "name": out.name, "ok": out.ok}
		if out.err != "" {
			item["error"] = out.err
		}
		return item
	}
	return object{"state": "idle"}
}

func (s *captureState) beginTranscribe(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transcribing == nil {
		s.transcribing = make(map[string]bool)
	}
	if s.transcribing[name] {
		return false
	}
	s.transcribing[name] = true
	return true
}

func (s *captureState) endTranscribe(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.transcribing, name)
}

func (s *captureState) computing(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transcribing[name]
}

func (b *larkServer) captureFiles(companionID string) ([]object, error) {
	entries, err := os.ReadDir(debugCaptureDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []object{}, nil
		}
		return nil, problem(502, "Cannot read captures")
	}
	files := make([]object, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		owner, ok := captureCompanion(name)
		if !ok || owner != companionID {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		id := captureID(name)
		path := filepath.Join(debugCaptureDir(), name)
		item := object{
			"name": id, "created": info.ModTime().UTC().Format(time.RFC3339),
			"size": info.Size(), "seconds": max(0, int((info.Size()-captureWAVHeader)/captureBytesPerSecond)),
			"computing": b.captures.computing(id),
		}
		if text := readCaptureTranscript(captureTranscriptPath(path)); text != "" {
			item["transcript"] = text
		}
		files = append(files, item)
	}
	sort.Slice(files, func(i, j int) bool {
		return infoTime(files[i]) > infoTime(files[j]) || (infoTime(files[i]) == infoTime(files[j]) && str(files[i]["name"]) > str(files[j]["name"]))
	})
	if len(files) > captureListMax {
		files = files[:captureListMax]
	}
	return files, nil
}

// readCaptureTranscript reads at most captureReadMax of a transcript so a
// damaged or substituted file cannot exhaust memory.
func readCaptureTranscript(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, captureReadMax))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func infoTime(item object) int64 {
	t, _ := time.Parse(time.RFC3339, str(item["created"]))
	return t.UnixNano()
}

func (b *larkServer) remoteCaptureStart(r *http.Request, companionID string) (any, error) {
	var req captureRequest
	if err := decodeRequest(r, &req); err != nil {
		return nil, err
	}
	run, err := b.startCapture(companionID, req, capturePageSeconds)
	if err != nil {
		return nil, err
	}
	b.captures.start(run)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(run.seconds)*time.Second+captureTimeoutSlack)
		defer cancel()
		result, err := b.waitCapture(ctx, run)
		if err == nil {
			_, err = b.saveCapture(run, result)
		}
		b.captures.finish(run, err)
	}()
	return object{"id": captureID(run.name), "name": captureID(run.name), "seconds": run.seconds,
		"started_at": run.started.UTC().Format(time.RFC3339)}, nil
}

func (b *larkServer) remoteCaptureList(companionID string) (any, error) {
	files, err := b.captureFiles(companionID)
	if err != nil {
		return nil, err
	}
	return object{"captures": files}, nil
}

func (b *larkServer) remoteCaptureTranscribe(r *http.Request, companionID, name string) (any, error) {
	path, ok := capturePath(companionID, name)
	if !ok {
		return nil, problem(404, "Capture not found")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, problem(404, "Capture not found")
	}
	if info, err := os.Stat(path); err != nil {
		return nil, problem(404, "Capture not found")
	} else if info.Size() > captureReadMax {
		return nil, problem(413, "Capture is too large to transcribe")
	}
	if !b.captures.beginTranscribe(name) {
		return nil, problem(409, "This capture is already being transcribed")
	}
	defer b.captures.endTranscribe(name)
	pcm, err := readWAVPCM16k(path)
	if err != nil {
		return nil, problem(400, "Cannot read capture: "+err.Error())
	}
	ctx, cancel := context.WithTimeout(r.Context(), captureTranscribeTimeout)
	defer cancel()
	start := time.Now()
	text, err := b.decodeClip(ctx, pcm)
	elapsed := time.Since(start)
	if err != nil {
		return nil, problem(502, err.Error())
	}
	text = strings.TrimSpace(text)
	if err := writePrivateFile(captureTranscriptPath(path), []byte(text+"\n")); err != nil {
		return nil, problem(500, "Cannot save transcript: "+err.Error())
	}
	return object{
		"name": name, "transcript": text, "stt_ms": milliseconds(elapsed),
		"audio_ms": len(pcm) * 1000 / captureBytesPerSecond, "engine": sttEngine,
	}, nil
}

func (b *larkServer) remoteCaptureWAV(w http.ResponseWriter, r *http.Request, companionID, name string) {
	path, ok := capturePath(companionID, name)
	if !ok {
		sendError(w, problem(404, "Capture not found"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		sendError(w, problem(404, "Capture not found"))
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		sendError(w, problem(404, "Capture not found"))
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func (b *larkServer) remoteCaptureDelete(w http.ResponseWriter, companionID, name string) {
	path, ok := capturePath(companionID, name)
	if !ok {
		sendError(w, problem(404, "Capture not found"))
		return
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			sendError(w, problem(404, "Capture not found"))
		} else {
			sendError(w, problem(500, "Cannot delete capture"))
		}
		return
	}
	if err := os.Remove(captureTranscriptPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		sendError(w, problem(500, "Cannot delete transcript"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sttAPI is the local-only transcription probe behind `lark stt-bench`. It
// accepts a WAV (16 kHz mono) or raw s16le PCM.
func (b *larkServer) sttAPI(r *http.Request) (any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, problem(400, "Cannot read audio")
	}
	pcm := raw
	if len(raw) >= captureWAVHeader && bytes.Equal(raw[:4], []byte("RIFF")) {
		pcm, err = parseWAVPCM16k(raw)
		if err != nil {
			return nil, problem(400, err.Error())
		}
	}
	if len(pcm) < 2 || len(pcm)%2 != 0 {
		return nil, problem(400, "Expected 16 kHz mono s16le PCM or WAV")
	}
	start := time.Now()
	text, err := b.decodeClip(r.Context(), pcm)
	elapsed := time.Since(start)
	if err != nil {
		return nil, problem(502, err.Error())
	}
	return object{
		"transcript": text,
		"stt_ms":     milliseconds(elapsed),
		"audio_ms":   len(pcm) * 1000 / captureBytesPerSecond,
		"engine":     sttEngine,
	}, nil
}

func captureUsage() error {
	return errors.New("usage: lark capture <companion> [seconds]")
}
