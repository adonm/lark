package main

import (
	"context"
	"crypto/sha256"
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
	pairPort        int
	worker          *synthWorker
	voices          companionVoices
	clips           speechClips
	audio           audioCache
	ota             otaFile
	box3OTA         otaFile
	ws              *wsHub
	tunnels         *tunnelManager
	history         *chatHistory
	timers          *timerManager
	builders        *buildManager
	portraits       *portraitStore
	diag            diagnostics
	companionMu     sync.Mutex // Serializes companion and voice writes through hub publication.
	companionTitles sync.Mutex

	parentToken string
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

type answer struct {
	Reply, SessionID string
	AudioURL         *string
}

func (b *larkServer) attachAudio(ctx context.Context, out *answer, stream *AudioStream, start time.Time) {
	if stream != nil {
		if pcm, err := stream.chunk(ctx, 0); err == nil && len(pcm) != 0 {
			stream.traceFirst(traceFrom(ctx), start)
			if id, _, _ := b.audio.add(stream); id != "" {
				path := "/api/audio/" + id
				out.AudioURL = &path
				traceFrom(ctx).firstPCM(time.Now())
			}
		}
	}
}

type delivery struct {
	sent, delivered, failed []string
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
	return others, directMessage(others, body), nil
}

func (b *larkServer) relay(ctx context.Context, sender companionState, others []companionTarget, target, text string) (answer, error) {
	recipients := b.intercomTargets(others, target)
	d := b.deliver(ctx, sender, recipients, text, nil)
	_, reply := intercomConfirmation(target, recipients, len(d.delivered) != 0, len(d.failed) != 0)
	sid, err := b.oc.get(sender.ID)
	if err != nil {
		return answer{}, err
	}
	out := answer{Reply: reply, SessionID: sid}
	start := time.Now()
	if b.worker != nil && b.worker.ready.Load() {
		if pcm, err := b.clips.get(ctx, b.worker, b.voiceFor(sender.ID), reply); err == nil {
			s := newAudioStream()
			s.append(pcm)
			s.finish(nil)
			b.attachAudio(ctx, &out, s, start)
		}
	}
	return out, nil
}

func (b *larkServer) turn(ctx context.Context, sender companionState, text string) (answer, error) {
	const source = "typed"
	if traceFrom(ctx) == nil {
		ctx = b.traceTurn(ctx, sender.ID, sender.Name, source, time.Now())
	}
	others, direct, err := b.intercomIntent(sender.ID, text)
	if err != nil {
		return answer{}, err
	}
	if direct != nil {
		b.chat(sender.ID, newChat("user", "You", text, source), true)
		return b.relay(ctx, sender, others, direct.target, direct.text)
	}
	if reply, _, err := b.buildReply(ctx, sender.ID, text); err != nil {
		return answer{}, err
	} else if reply != "" {
		out := answer{Reply: reply}
		if w := b.worker; w != nil && w.ready.Load() {
			start := time.Now()
			audioCtx, cancel := context.WithTimeout(ctx, ttsWait)
			defer cancel()
			speech := newSpeechTurn(audioCtx, cancel, w, b.voiceFor(sender.ID), nil)
			speech.complete([]string{reply})
			b.attachAudio(ctx, &out, speech.stream, start)
		}
		if err := b.deliverOfflineBuildReply(ctx, sender, text, reply, source); err != nil {
			return answer{}, err
		}
		return out, nil
	}
	options := turnOptions{source: source}
	gen, err := b.generateReply(ctx, b.root, sender, text, options)
	if err != nil {
		return answer{}, err
	}
	out := answer{Reply: gen.reply, SessionID: gen.sid}
	if gen.reply != "" {
		b.attachAudio(ctx, &out, gen.audio, gen.ttsStart)
	}
	return out, nil
}

type generatedReply struct {
	reply, sid string
	audio      *AudioStream
	ttsStart   time.Time
}

type turnOptions struct {
	source  string
	web     bool
	scoped  bool
	publish func(*AudioStream, time.Time)
}

func turnPrompt(companionID string, web bool, source, text string, companion companionState) string {
	kind := "hardware"
	if web {
		kind = "web"
	}
	extra := ""
	if !companion.Configured {
		extra += " · setup"
	}
	if companion.Personality != "" {
		extra += " · vibe: " + companion.Personality
	}
	return fmt.Sprintf("[%s (%s) · %s · %s%s]\n%s", companion.Name, companionID, kind, source, extra, text)
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
	onSentence := func(s string) {
		start := time.Now()
		clean, err := spokenText(s)
		if err != nil || b.worker == nil || !b.worker.ready.Load() {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if eager == nil {
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
		eager.send(clean)
	}
	pieces := turnStream{sentence: onSentence, tool: func() {
		mu.Lock()
		defer mu.Unlock()
		drop(true)
	}}
	prepare := func() (string, string) {
		companion := b.companionFor(sender.ID)
		return companion.Name, turnPrompt(sender.ID, options.web, options.source, text, companion)
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
	var stream *AudioStream
	clean, speechErr := spokenText(reply)
	if speechErr == nil && (eager != nil || b.worker != nil && b.worker.ready.Load()) {
		final := SplitSentences(clean)
		if eager == nil || !eager.complete(final) {
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
	case path == "/api/parent":
		return b.routeParent(w, r, "")
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/portraits/"):
		return nil, b.servePortrait(w, r, path, "")
	case path == "/api/companions" || strings.HasPrefix(path, "/api/companions/"):
		return b.companionAPI(w, r, path)
	case path == "/api/terminals" || strings.HasPrefix(path, "/api/terminals/"):
		return b.terminalAPI(w, r, path)
	case path == "/api/memory" || path == "/api/memory/scopes":
		return b.memoryAPI(w, r, path)
	case path == "/api/apps" || strings.HasPrefix(path, "/api/apps/"):
		return b.appAPI(w, r, path)
	case path == "/api/timers" || strings.HasPrefix(path, "/api/timers/"):
		return b.timerAPI(w, r, path, "")
	case path == "/api/tunnel/status" || path == "/api/tunnels" || strings.HasPrefix(path, "/api/tunnels/"):
		return b.tunnelAPI(w, r, path)
	case r.Method == http.MethodPost && path == "/api/chat":
		return b.chatAPI(r, "")
	}
	switch r.Method {
	case http.MethodGet:
		switch {
		case path == "/":
			http.Redirect(w, r, "/app", http.StatusFound)
			return nil, nil
		case path == "/app/parent":
			if !parentLAN(r) {
				return nil, problem(403, "Open parent controls directly on your LAN")
			}
			serveAppFile(w, "parent.html", "text/html; charset=utf-8")
			return nil, nil
		case b.servePWA(w, r, path, nil, "") || serveApp(w, path):
			return nil, nil
		case path == "/ws" && b.ws != nil:
			return nil, b.ws.serve(w, r)
		case path == "/health":
			o := b.oc != nil && b.oc.reachable(ctx, "/api/info")
			ready := b.worker != nil && b.worker.ready.Load()
			engine := "unavailable"
			if ready {
				engine = kokoroEngine
			}
			return object{"ok": true, "stt": b.stt != nil, "stt_engine": sttEngine,
				"opencode": o, "tts": ready, "tts_engine": engine}, nil
		case path == "/api/diagnostics":
			return b.diagnosticReport(), nil
		case path == "/api/scope":
			b.setPWACookie(w)
			return object{"scoped": false, "auto_volume": true, "voices": voiceCatalog}, nil
		case strings.HasPrefix(path, "/api/audio/"):
			return nil, b.audio.ServeAudio(w, r, strings.TrimPrefix(path, "/api/audio/"))
		case path == "/ota/version.json" || path == "/ota/latest.bin":
			return b.ota.serve(w, path == "/ota/version.json")
		case path == "/ota/box3/version.json" || path == "/ota/box3/latest.bin":
			return b.box3OTA.serve(w, path == "/ota/box3/version.json")
		}
	case http.MethodPost:
		if path == "/api/diagnostics/stt" {
			return b.probeSTT(r)
		}
		if path == "/api/opencode/pair" {
			return b.pairOpenCode(r)
		}
		if path == "/api/opencode/compact" {
			return b.compactAPI(w, r)
		}
		if path == "/api/tts" {
			obj, err := jsonBody(r)
			if err != nil {
				return nil, err
			}
			text, err := spokenText(str(obj["text"]))
			if err != nil {
				return nil, err
			}
			if b.worker == nil || !b.worker.ready.Load() {
				return nil, problem(503, "TTS unavailable")
			}
			id, err := companionIDArg(r, obj, false)
			if err != nil {
				return nil, err
			}
			if id != "" {
				if err := b.parentAllowed(id); err != nil {
					return nil, err
				}
			}
			voice := b.voices.defaultFor("")
			if id != "" {
				voice = b.voiceFor(id)
			}
			ctx, cancel := context.WithTimeout(ctx, ttsWait)
			defer cancel()
			pcm, err := b.worker.synthesize(ctx, voice, text)
			if err != nil {
				return nil, problem(502, "TTS synthesis failed")
			}
			sendBytes(w, 200, pcmType, pcm)
			return nil, nil
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
