package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type larkServer struct {
	db              *larkDB
	root            context.Context
	token           string
	terminals       *store[terminalRecord]
	stt             func(context.Context, []byte) (string, error)
	endpoint        *endpointSettings
	oc              *openCode
	worker          *synthWorker
	voices          companionVoices
	clips           speechClips
	ota             otaFile
	box3OTA         otaFile
	ws              *wsHub
	dc              *dcService
	history         *chatHistory
	timers          *timerManager
	builders        *buildManager
	portraits       *portraitStore
	diag            diagnostics
	loc             *time.Location
	companionMu     sync.Mutex // Serializes companion and voice writes through hub publication.
	companionTitles sync.Mutex
	dcBuildMu       sync.Mutex
	dcBuildTargets  map[string]dcBuildTarget
}

func (b *larkServer) location() *time.Location {
	if b != nil && b.loc != nil {
		return b.loc
	}
	return time.Local
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sendBytes(w http.ResponseWriter, status int, kind string, data []byte) {
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func sendJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, status = []byte(`{"error":"Cannot encode response"}`), 500
	}
	sendBytes(w, status, jsonType, data)
}

func tokenMatches(values []string, want string) bool {
	if want == "" || len(values) != 1 {
		return false
	}
	wantHash := sha256.Sum256([]byte(want))
	gotHash := sha256.Sum256([]byte(values[0]))
	return subtle.ConstantTimeCompare(gotHash[:], wantHash[:]) == 1
}

func tokenHeaderMatches(h http.Header, name, want string) bool {
	return tokenMatches(h.Values("X-Lark-"+name), want)
}

func (b *larkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	w.Header().Set("Cache-Control", "no-store")
	if path == "/mcp" {
		b.serveMCP(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ota/") {
		if !tokenHeaderMatches(r.Header, "Token", b.token) {
			sendError(w, problem(401, "Unauthorized"))
			return
		}
	}
	value, err := b.route(w, r, path)
	sendResult(w, value, err)
}

func sendResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		sendError(w, err)
	} else if value != nil {
		sendJSON(w, 200, value)
	}
}

func sendError(w http.ResponseWriter, err error) {
	status := 502
	var api *apiError
	var upstream *upstreamStatus
	if errors.As(err, &api) {
		status = api.status
	} else if errors.As(err, &upstream) {
		status = upstream.status
	}
	sendJSON(w, status, object{"error": err.Error()})
}

func jsonBody(r *http.Request) (object, error) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != jsonType {
		return nil, problem(415, "Expected application/json")
	}
	lengths := r.Header.Values("Content-Length")
	if len(r.TransferEncoding) != 0 || len(lengths) > 1 {
		return nil, problem(400, "Request requires a single Content-Length")
	}
	if len(lengths) == 0 && r.ContentLength <= 0 {
		return nil, problem(411, "Content-Length required")
	}
	if len(lengths) != 0 && len(lengths[0]) > 12 {
		return nil, problem(400, "Invalid Content-Length")
	}
	if r.ContentLength > maxJSON {
		return nil, problem(413, "Request body too large")
	}
	if r.ContentLength < 0 {
		return nil, problem(400, "Invalid Content-Length")
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		var timed net.Error
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, problem(408, "Request body timed out")
		}
		return nil, problem(400, "Incomplete request body")
	}
	return decodeObject(buf)
}

func companionIDArg(r *http.Request, obj object, required bool) (string, error) {
	v, provided := obj["companion_id"]
	if !provided && !required {
		v = ""
		if values := r.Header.Values("X-Companion-Id"); len(values) > 0 {
			v = values[0]
		}
	}
	id := strings.ToLower(strings.TrimSpace(str(v)))
	if id == "" && !required {
		return "", nil
	}
	if !validCompanionID(id) {
		return "", problem(400, "Invalid companion ID")
	}
	return id, nil
}

func (b *larkServer) voiceFor(companionID string) string {
	c := b.companionFor(companionID)
	return b.voices.resolve(c.Voice, companionTheme(c.Name))
}

func (b *larkServer) volumeAPI(r *http.Request, id string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	auto, ok := args["auto"]
	if !ok || len(args) > 2 || len(args) == 2 && args["volume"] == nil {
		return nil, problem(400, `Expected {"auto":true} or {"auto":false,"volume":0..100}`)
	}
	if b.ws == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	delete(args, "auto")
	args["auto_volume"] = auto
	return b.ws.terminalVolumeControl(r.Context(), id, args)
}

func (b *larkServer) voiceAPI(r *http.Request, id string) (any, error) {
	obj, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	voice := str(obj["voice"])
	if !b.voices.allowed(voice) {
		return nil, problem(400, "Unsupported voice")
	}
	save := b.voices.save
	if b.ws != nil {
		save = b.ws.saveVoice
	}
	if err := save(id, voice); err != nil {
		return nil, err
	}
	return object{"companion_id": id, "voice": voice}, nil
}

func intercomConfirmation(target string, recipients []companionTarget, sent, partial bool) (title, reply string) {
	if broadcast(target) {
		target = "everyone"
	} else if len(recipients) != 0 {
		target = recipients[0].Name
	}
	switch {
	case !sent:
		return "Not delivered", "I couldn't reach " + target + " right now."
	case partial:
		return "Partly delivered", "I sent your message, but couldn't reach everyone."
	default:
		return "Message sent", "Sent to " + target + "!"
	}
}

func (b *larkServer) intercomIntent(id, text string) ([]companionTarget, *intercomMessage, error) {
	body := directBody(text)
	if body == "" {
		return nil, nil, nil
	}
	others, err := b.peerSnapshot(id)
	if err != nil {
		return nil, nil, err
	}
	if b.dc != nil && b.dc.ready.Load() {
		if paired, err := b.dc.listPairedContacts(id); err == nil {
			for _, ct := range paired {
				others = append(others, companionTarget{ID: ct.Name, Name: ct.Name})
			}
		}
	}
	return others, directMessage(others, body), nil
}

type delivery struct {
	sent, delivered, failed []string
}

type generatedReply struct {
	reply, sid string
	audio      *AudioStream
	ttsStart   time.Time
}

type turnChannel struct {
	channel    string // "in_person", "deltachat", "timer"
	sender     string
	senderKind string // "contact", "companion"
	replyTo    string // Delta Chat sender name when replying in listening mode
	private    bool
}

type turnOptions struct {
	source  string
	channel turnChannel
	publish func(*AudioStream, time.Time)
}

func formatLocalTime(t time.Time) string {
	return formatLocalTimeIn(t, time.Local)
}

func formatLocalTimeIn(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format("2006-01-02 15:04 MST (-07:00)")
}

func turnHeader(companionID, source string, companion companionState, ch turnChannel, now time.Time) string {
	var prefix string
	switch {
	case ch.channel == "deltachat":
		kind := ch.senderKind
		if kind == "" {
			kind = "contact"
		}
		sender := ch.sender
		if sender == "" {
			sender = "Friend"
		}
		if ch.private {
			prefix = fmt.Sprintf("Delta Chat from %s (%s) · private · %s (%s)", sender, kind, companion.Name, companionID)
		} else {
			prefix = fmt.Sprintf("Delta Chat from %s (%s) · %s (%s)", sender, kind, companion.Name, companionID)
		}
	case ch.channel == "timer" || source == "timer":
		prefix = fmt.Sprintf("Timer · %s (%s)", companion.Name, companionID)
	case ch.replyTo != "":
		prefix = fmt.Sprintf("In person at %s's device · replying to %s (Delta Chat) · %s (%s) · %s", companion.Name, ch.replyTo, companion.Name, companionID, source)
	default:
		prefix = fmt.Sprintf("In person at %s's device · %s (%s) · %s", companion.Name, companion.Name, companionID, source)
	}
	if !companion.Configured {
		extra := " · setup"
		if companion.Personality != "" {
			extra += " · vibe: " + companion.Personality
		}
		return "[" + prefix + extra + "]"
	}
	if companion.Personality != "" {
		return "[" + prefix + " · vibe: " + companion.Personality + "]"
	}
	return "[" + prefix + "]"
}

func turnStateLine(now time.Time, companion companionState, terminals, timers, books string) string {
	parts := []string{"now: " + formatLocalTimeIn(now, now.Location())}
	if companion.ListenSeconds > 0 {
		parts = append(parts, fmt.Sprintf("listen: %ds", companion.ListenSeconds))
	} else {
		parts = append(parts, "listen: off")
	}
	if terminals != "" {
		parts = append(parts, "terminals: "+terminals)
	}
	if timers != "" {
		parts = append(parts, "timers: "+timers)
	}
	if books != "" {
		parts = append(parts, "books: "+books)
	}
	return "[state: " + strings.Join(parts, " · ") + "]"
}

func turnPrompt(companionID string, source, text string, companion companionState) string {
	return turnPromptAt(companionID, source, text, companion, time.Now())
}

func turnPromptAt(companionID string, source, text string, companion companionState, now time.Time) string {
	return turnPromptChannelAt(companionID, source, text, companion, turnChannel{}, now)
}

func turnPromptChannelAt(companionID, source, text string, companion companionState, ch turnChannel, now time.Time) string {
	return turnHeader(companionID, source, companion, ch, now) + "\n" + text + "\n" + turnStateLine(now, companion, "", "", "")
}

func (b *larkServer) turnPromptWithState(ctx context.Context, companionID, source, text string, companion companionState, ch turnChannel, now time.Time) string {
	var termParts []string
	if terms, err := b.terminalList(false); err == nil {
		for _, t := range terms {
			if str(t["companion"]) != companionID {
				continue
			}
			status := "offline"
			if b, _ := t["online"].(bool); b {
				status = "online"
			}
			vol := fmt.Sprintf("vol:%v", t["volume"])
			if auto, _ := t["auto_volume"].(bool); auto {
				vol += ",auto"
			}
			termParts = append(termParts, fmt.Sprintf("%s(%s,%s)", str(t["id"]), status, vol))
		}
	}
	terminals := "none"
	if len(termParts) > 0 {
		terminals = strings.Join(termParts, ",")
	}

	var timerParts []string
	if b.timers != nil {
		if list, err := b.timers.list(ctx, companionID); err == nil {
			for _, tm := range list {
				rem := max(0, int(tm.FireAt.Sub(now).Round(time.Second)/time.Second))
				if tm.AppName != "" {
					timerParts = append(timerParts, fmt.Sprintf("%s(%s,%ds)", tm.ID, tm.AppName, rem))
				} else if tm.Label != "" {
					timerParts = append(timerParts, fmt.Sprintf("%s(%q,%ds)", tm.ID, tm.Label, rem))
				} else {
					timerParts = append(timerParts, fmt.Sprintf("%s(%ds)", tm.ID, rem))
				}
			}
		}
	}
	timers := "none"
	if len(timerParts) > 0 {
		timers = strings.Join(timerParts, ",")
	}

	var bookParts []string
	if apps, err := b.listApps(ctx, false); err == nil {
		for _, a := range apps {
			if str(a["kind"]) != "book" {
				continue
			}
			slug := str(a["name"])
			title := str(a["title"])
			if title != "" && title != slug {
				bookParts = append(bookParts, fmt.Sprintf("%s(%q)", slug, title))
			} else {
				bookParts = append(bookParts, slug)
			}
		}
	}
	books := "none"
	if len(bookParts) > 0 {
		books = strings.Join(bookParts, ",")
	}

	return turnHeader(companionID, source, companion, ch, now) + "\n" + text + "\n" + turnStateLine(now, companion, terminals, timers, books)
}

func isSilentReply(s string) bool {
	t := strings.TrimSpace(s)
	t = strings.TrimRight(t, ".!?")
	return t == "" || strings.EqualFold(t, "[silent]")
}

// Play plain prose as soon as the first PCM is ready; reconcile against the
// authoritative reply before finishing the stream.
func (b *larkServer) generateReply(ctx, audioBase context.Context, sender companionState, text string, options turnOptions) (generatedReply, error) {
	if traceFrom(ctx) == nil {
		ctx = b.traceTurn(ctx, sender.ID, sender.Name, options.source, time.Now())
	}
	makeSpeech := func(onFirst func(*AudioStream)) *speechTurn {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(turnTimeout)
		}
		audioCtx, cancel := context.WithDeadline(context.WithValue(audioBase, traceKey{}, traceFrom(ctx)), deadline)
		return newSpeechTurn(audioCtx, cancel, b.worker, b.voiceFor(sender.ID), onFirst)
	}
	var mu sync.Mutex
	var eager *speechTurn
	var ttsStart time.Time
	published := false
	spoken := false
	preToolCount := 0
	drop := func(abort bool) {
		if eager == nil {
			return
		}
		if abort && published {
			options.publish(nil, time.Time{})
		}
		eager.discard()
		eager = nil
		published = false
	}
	ensureEager := func(start time.Time) {
		if eager != nil {
			return
		}
		ttsStart = start
		eager = makeSpeech(func(stream *AudioStream) {
			mu.Lock()
			defer mu.Unlock()
			if options.publish != nil && !published && eager != nil && eager.stream == stream && ctx.Err() == nil {
				options.publish(stream, start)
				published = true
			}
		})
	}
	onSentence := func(s string) {
		if isSilentReply(s) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "[silent") {
			return
		}
		start := time.Now()
		clean, err := spokenText(s)
		if err != nil || b.worker == nil || !b.worker.ready.Load() {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		spoken = true
		ensureEager(start)
		eager.send(clean)
	}
	pieces := turnStream{sentence: onSentence, tool: func() {
		mu.Lock()
		defer mu.Unlock()
		if eager != nil {
			preToolCount = len(eager.sent)
			return
		}
		if !spoken && b.worker != nil && b.worker.ready.Load() {
			if pcm := b.clips.peekAny(b.voiceFor(sender.ID), thinkingFillers, uint64(time.Now().UnixNano())); len(pcm) > 0 {
				ensureEager(time.Now())
				eager.sendPCM(pcm)
			}
		}
	}}
	prepare := func() (string, string) {
		companion := b.companionFor(sender.ID)
		return companion.Name, b.turnPromptWithState(ctx, sender.ID, options.source, text, companion, options.channel, time.Now().In(b.location()))
	}
	raw, sid, err := b.oc.askPrepared(ctx, sender.ID, prepare, pieces, options.source,
		nil, func(entry chatEntry) {
			if entry.Role == "assistant" {
				entry.Sender = b.companionFor(sender.ID).Name
			}
			b.chat(sender.ID, entry, true)
		})
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		drop(true)
		return generatedReply{}, err
	}
	reply := strings.TrimSpace(fenced.ReplaceAllString(raw, ""))
	if isSilentReply(reply) {
		drop(true)
		return generatedReply{"", sid, nil, time.Time{}}, nil
	}
	var stream *AudioStream
	clean, speechErr := spokenText(reply)
	if speechErr == nil && (eager != nil || b.worker != nil && b.worker.ready.Load()) {
		var ex sentenceExtractor
		final := append(ex.add(clean), ex.flush()...)
		if eager == nil || !eager.completeAfter(preToolCount, final) {
			drop(false) // Publishing its replacement invalidates the old generation.
			ttsStart = time.Now()
			eager = makeSpeech(nil)
			eager.complete(final)
		}
		stream = eager.stream
		if options.publish != nil && !published {
			options.publish(stream, ttsStart)
			published = true
		}
	} else {
		drop(true)
	}
	return generatedReply{reply, sid, stream, ttsStart}, nil
}

func (b *larkServer) route(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/companions" || strings.HasPrefix(path, "/api/companions/"):
		return b.companionAPI(w, r, path)
	case path == "/api/terminals" || strings.HasPrefix(path, "/api/terminals/"):
		return b.terminalAPI(w, r, path)
	}
	if r.Method == http.MethodGet {
		switch {
		case path == "/health":
			o := b.oc != nil && b.oc.reachable(ctx, "/api/info")
			ready := b.worker != nil && b.worker.ready.Load()
			engine := "unavailable"
			if ready {
				engine = kokoroEngine
			}
			return object{"ok": true, "stt": b.stt != nil, "stt_engine": sttEngine,
				"opencode": o, "tts": ready, "tts_engine": engine,
				"deltachat": b.dc != nil && b.dc.ready.Load()}, nil
		case path == "/api/diagnostics":
			return b.diagnosticReport(), nil
		case path == "/ota/version.json" || path == "/ota/latest.bin":
			return b.ota.serve(w, path == "/ota/version.json")
		case path == "/ota/box3/version.json" || path == "/ota/box3/latest.bin":
			return b.box3OTA.serve(w, path == "/ota/box3/version.json")
		}
	}
	return nil, problem(404, "Not found")
}

type otaFile struct {
	path   string
	mu     sync.Mutex
	info   os.FileInfo
	digest string
}

func (o *otaFile) sha(f *os.File, info os.FileInfo) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.info != nil && info.Size() == o.info.Size() && info.ModTime() == o.info.ModTime() && os.SameFile(info, o.info) {
		return o.digest, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, info.Size()))
	if err != nil || n != info.Size() {
		return "", problem(502, "Cannot read firmware")
	}
	o.info, o.digest = info, hex.EncodeToString(h.Sum(nil))
	return o.digest, nil
}

func (o *otaFile) serve(w http.ResponseWriter, manifest bool) (any, error) {
	f, err := os.Open(o.path)
	if err != nil {
		if os.IsNotExist(err) && manifest {
			return object{"exists": false, "size": 0, "sha256": "", "mtime": 0}, nil
		}
		return nil, problem(404, "Firmware not found")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, problem(404, "Firmware not found")
	}
	digest, err := o.sha(f, info)
	if err != nil {
		return nil, err
	}
	if manifest {
		return object{"exists": true, "size": info.Size(), "sha256": digest, "mtime": info.ModTime().Unix()}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, problem(502, "Cannot read firmware")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Firmware-Sha256", digest)
	w.WriteHeader(200)
	_, _ = io.CopyN(w, f, info.Size())
	return nil, nil
}
