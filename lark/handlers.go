package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/pprof"
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
	sttMu           sync.RWMutex
	stt             func(context.Context, []byte) (string, error)
	sttLabel        string
	ttsLabel        string
	models          modelSet
	endpoint        *endpointSettings
	oc              *openCode
	worker          *synthWorker
	voices          companionVoices
	clips           speechClips
	ota             otaFile
	box3OTA         otaFile
	hub             *terminalHub
	dc              *dcService
	history         *chatHistory
	timers          *timerManager
	portraits       *portraitStore
	library         string
	libraryStats    libraryStats
	libraryMu       sync.Mutex
	libraryDBs      map[string]*sql.DB
	remote          *remoteManager
	captures        captureState
	diag            diagnostics
	loc             *time.Location
	companionMu     sync.Mutex // Serializes companion and voice writes through hub publication.
	companionTitles sync.Mutex
	storyBGMu       sync.Mutex
	storyBGInflight map[string]bool
	storyCache      storyCompileCache
	portraitBGMu    sync.Mutex
	portraitBG      map[string]*portraitBGQueue
	emits           emitTracker // Successful emit calls, to detect explicit reply routing.
}

func (b *larkServer) location() *time.Location {
	if b != nil && b.loc != nil {
		return b.loc
	}
	return time.Local
}

// randomHex mints an opaque identifier. Callers must fail the operation when
// the system entropy source fails; there is no predictable fallback.
func randomHex(n int) (string, error) {
	return randomHexFrom(rand.Reader, n)
}

func randomHexFrom(r io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("secure random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// loopbackRequest reports whether the connection came from this host. The
// HTTP listener is LAN-reachable for firmware OTA, so admin routes must check
// the peer address as well as the token.
func loopbackRequest(r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		if !loopbackRequest(r) {
			sendError(w, problem(404, "Not found"))
			return
		}
		b.serveMCP(w, r)
		return
	}
	if strings.HasPrefix(path, "/debug/pprof") {
		if !loopbackRequest(r) {
			sendError(w, problem(404, "Not found"))
			return
		}
		if !tokenHeaderMatches(r.Header, "Token", b.token) {
			sendError(w, problem(401, "Unauthorized"))
			return
		}
		switch path {
		case "/debug/pprof/cmdline":
			pprof.Cmdline(w, r)
		case "/debug/pprof/profile":
			pprof.Profile(w, r)
		case "/debug/pprof/symbol":
			pprof.Symbol(w, r)
		case "/debug/pprof/trace":
			pprof.Trace(w, r)
		default:
			pprof.Index(w, r)
		}
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

func (b *larkServer) voiceFor(companionID string) string {
	c := b.companionFor(companionID)
	return b.voices.resolve(c.Voice, companionTheme(c.Name))
}

func (b *larkServer) volumeAPI(r *http.Request, id string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	level, ok := args["volume"]
	if !ok || len(args) != 1 || level == nil {
		return nil, problem(400, `Expected {"volume":0..100}`)
	}
	if b.hub == nil {
		return nil, problem(503, "Terminal hub unavailable")
	}
	return b.hub.terminalVolumeControl(r.Context(), id, args)
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
	if b.hub != nil {
		save = b.hub.saveVoice
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

func (b *larkServer) ttsAPI(w http.ResponseWriter, r *http.Request) error {
	obj, err := jsonBody(r)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(str(obj["text"]))
	if text == "" {
		return problem(400, "Expected non-empty text")
	}
	voice := strings.TrimSpace(str(obj["voice"]))
	if voice == "" {
		voice = heartVoice
	}
	if b.worker == nil {
		return problem(503, "TTS unavailable")
	}
	pcm, err := b.worker.synthesize(r.Context(), voice, text)
	if err != nil {
		return problem(503, err.Error())
	}
	hdr := wavHeader(len(pcm))
	out := make([]byte, len(hdr)+len(pcm))
	copy(out, hdr[:])
	copy(out[len(hdr):], pcm)
	sendBytes(w, 200, "audio/wav", out)
	return nil
}

func (b *larkServer) route(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	if value, err, handled := stubHTTP(b, r, path); handled {
		return value, err
	}
	switch {
	case path == "/remote" || strings.HasPrefix(path, "/remote/"):
		b.remoteHTTP(w, r, path)
		return nil, nil
	case path == "/api/remote/tunnels" || strings.HasPrefix(path, "/api/remote/tunnels/"):
		return b.remoteTunnelAPI(w, r, path)
	case path == "/api/companions" || strings.HasPrefix(path, "/api/companions/"):
		return b.companionAPI(w, r, path)
	case path == "/api/terminals" || strings.HasPrefix(path, "/api/terminals/"):
		return b.terminalAPI(w, r, path)
	case path == "/api/tts" && r.Method == http.MethodPost:
		return nil, b.ttsAPI(w, r)
	case path == "/api/capture" && r.Method == http.MethodPost:
		return b.captureAPI(r)
	case path == "/api/stt" && r.Method == http.MethodPost:
		return b.sttAPI(r)
	}
	if r.Method == http.MethodGet {
		switch {
		case path == "/health":
			o := b.oc != nil && b.oc.reachable(ctx, "/api/info")
			ready := b.worker != nil && b.worker.ready.Load()
			engine := "unavailable"
			if ready {
				engine = b.ttsLabel
				if engine == "" {
					engine = kokoroEngine
				}
			}
			sttName := b.sttLabel
			if sttName == "" {
				sttName = sttEngine
			}
			out := object{"ok": true, "stt": b.sttFunc() != nil, "stt_engine": sttName,
				"opencode": o, "tts": ready, "tts_engine": engine,
				"smart_turn": b.endpoint.smart() != nil,
				"deltachat":  b.dc != nil && b.dc.ready.Load()}
			if b.models.stt != nil {
				out["models"] = b.models.report()
				out["models_ready"] = b.models.allReady()
			}
			return out, nil
		case path == "/api/diagnostics":
			return b.diagnosticReport(), nil
		case path == "/api/library":
			return libraryPlanReport(b.libraryDir()), nil
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
