package edge

// The brain Protocol
// -----------------
// One JSON object per line over a 0600 Unix socket. Edge -> brain messages
// carry "type"; brain -> edge messages carry "cmd" and an optional "id" for a
// request/response RPC. Replies echo "id" and set "ok".
//
// Edge events:
//   ready             {models: {parakeet,kokoro,smart_turn,silero}}
//   terminal.connected {terminal_id,board,firmware,width,height,shape,ip}
//   terminal.disconnected {terminal_id}
//   terminal.telemetry {terminal_id,wifi_rssi,battery_pct,charging,cpu_pct,free_heap,uptime_s}
//   turn.speculate    {terminal_id,seq,transcript,speech_at_ms}
//   turn.commit       {terminal_id,seq,transcript,speech_at_ms,listen_window,audio_ms}
//   turn.cancel       {terminal_id,seq,reason}
//   turn.not_caught   {terminal_id}
//   pointer.action    {terminal_id,action,value}
//   playback.done     {terminal_id,stream_id,aborted,underruns_delta}
//   capture.progress  {terminal_id,capture_id,remaining_s}
//   capture.done      {terminal_id,capture_id,path,seconds,error?}
//
// Commands: terminal.render, terminal.speak_start, terminal.speak_chunk,
// terminal.speak_end, terminal.filler, terminal.preview_voice,
// terminal.stop_audio, terminal.chime, terminal.power, terminal.volume,
// terminal.metronome, terminal.play_tune, terminal.rec_start,
// terminal.rec_cancel, terminal.hits (RPC),
// stt.transcribe (RPC), portrait.normalize (RPC), capture.start (RPC),
// capture.stop (RPC), health (RPC).
//
// A new brain connection immediately receives ready plus one
// terminal.connected snapshot per live board, so restarting the brain never
// disturbs a hardware TCP session.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lark/internal/audio"
	"lark/internal/pixel"
)

type ipcCommand struct {
	ID              string           `json:"id"`
	Cmd             string           `json:"cmd"`
	TerminalID      string           `json:"terminal_id"`
	StreamID        uint64           `json:"stream_id"`
	Voice           string           `json:"voice"`
	Text            string           `json:"text"`
	Path            string           `json:"path"`
	Dir             string           `json:"dir"`
	OutPath         string           `json:"out_path"`
	Screen          json.RawMessage  `json:"screen"`
	CaptureID       string           `json:"capture_id"`
	Seconds         int              `json:"seconds"`
	ListenWindow    bool             `json:"listen_window"`
	WindowS         int              `json:"window_s"`
	FollowUpGraceMS int              `json:"follow_up_grace_ms"`
	CompanionName   string           `json:"companion_name"`
	Kind            string           `json:"kind"`
	Level           int              `json:"level"`
	Brightness      int              `json:"brightness"`
	BPM             int              `json:"bpm"`
	Beats           int              `json:"beats"`
	DurationS       int              `json:"duration_s"`
	Tempo           int              `json:"tempo"`
	Style           string           `json:"style"`
	Notes           []audio.TuneNote `json:"notes"`
}

type ipcClient struct {
	e     *Edge
	conn  net.Conn
	out   chan []byte
	done  chan struct{}
	once  sync.Once
	write sync.WaitGroup
}

func (c *ipcClient) send(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	b = append(b, '\n')
	select {
	case c.out <- b:
	case <-c.done:
	default:
		// A stalled brain must not stall hardware timing. Drop the event.
	}
}

func (c *ipcClient) reply(id string, fields map[string]any) {
	if id == "" {
		return
	}
	msg := make(map[string]any, len(fields)+2)
	msg["id"], msg["ok"] = id, true
	for k, v := range fields {
		msg[k] = v
	}
	c.send(msg)
}

func (c *ipcClient) fail(id string, err error) {
	if id == "" || err == nil {
		return
	}
	c.send(map[string]any{"id": id, "ok": false, "error": err.Error()})
}

func (c *ipcClient) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

type ipcServer struct {
	e      *Edge
	path   string
	ln     net.Listener
	mu     sync.Mutex
	conns  map[*ipcClient]struct{}
	closed bool
	wg     sync.WaitGroup
}

func newIPCServer(e *Edge, path string) (*ipcServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("edge socket path %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &ipcServer{e: e, path: path, ln: ln, conns: make(map[*ipcClient]struct{})}, nil
}

func (s *ipcServer) start() {
	s.wg.Add(1)
	go s.accept()
}

func (s *ipcServer) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]*ipcClient, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	for _, c := range conns {
		c.close()
	}
	s.wg.Wait()
	_ = os.Remove(s.path)
}

func (s *ipcServer) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed {
				return
			}
			continue
		}
		c := &ipcClient{e: s.e, conn: conn, out: make(chan []byte, 256), done: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.e.addBrain(c)
		s.e.snapshot(c)
		c.write.Add(1)
		go s.writer(c)
		s.wg.Add(1)
		go s.reader(c)
	}
}

func (s *ipcServer) writer(c *ipcClient) {
	defer c.write.Done()
	w := bufio.NewWriter(c.conn)
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			if _, err := w.Write(b); err != nil {
				c.close()
				return
			}
			if len(c.out) == 0 {
				if err := w.Flush(); err != nil {
					c.close()
					return
				}
			}
		}
	}
}

func (s *ipcServer) reader(c *ipcClient) {
	defer s.wg.Done()
	defer func() {
		s.e.removeBrain(c)
		c.close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	dec := json.NewDecoder(bufio.NewReader(c.conn))
	dec.UseNumber()
	for {
		var cmd ipcCommand
		if err := dec.Decode(&cmd); err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// A malformed line ends the connection; the brain can reconnect
				// and receives a fresh snapshot.
				c.fail(cmd.ID, err)
			}
			return
		}
		s.e.handleCommand(c, cmd)
	}
}

func (e *Edge) handleCommand(c *ipcClient, cmd ipcCommand) {
	if err := e.ensureOpen(); err != nil {
		c.fail(cmd.ID, err)
		return
	}
	switch cmd.Cmd {
	case "health":
		terms := e.Terminals()
		list := make([]map[string]any, 0, len(terms))
		for _, t := range terms {
			list = append(list, map[string]any{
				"terminal_id": t.id,
				"connected":   t.native() != nil,
				"reconnects":  t.reconnects(),
				"firmware":    t.firmware,
			})
		}
		c.reply(cmd.ID, map[string]any{
			"models": e.models.snapshot(), "terminals": len(terms), "terminal_list": list,
		})
	case "stt.transcribe":
		text, err := e.transcribeFile(cmd.Path)
		if err != nil {
			c.fail(cmd.ID, err)
			return
		}
		c.reply(cmd.ID, map[string]any{"text": text})
	case "tts.wav":
		path, err := e.synthesizeWAV(cmd.Voice, cmd.Text, cmd.OutPath)
		if err != nil {
			c.fail(cmd.ID, err)
			return
		}
		c.reply(cmd.ID, map[string]any{"ok": true, "path": path})
	case "portrait.normalize":
		colors, err := normalizePortraitDir(cmd.Dir)
		if err != nil {
			c.fail(cmd.ID, err)
			return
		}
		c.reply(cmd.ID, map[string]any{"ok": true, "colors": colors})
	default:
		e.handleTerminalCommand(c, cmd)
	}
}

func (e *Edge) handleTerminalCommand(c *ipcClient, cmd ipcCommand) {
	t := e.Terminal(cmd.TerminalID)
	if t == nil {
		c.fail(cmd.ID, errors.New("terminal not connected"))
		return
	}
	switch cmd.Cmd {
	case "terminal.render":
		var s Screen
		if len(cmd.Screen) > 0 {
			if err := json.Unmarshal(cmd.Screen, &s); err != nil {
				c.fail(cmd.ID, err)
				return
			}
		}
		t.event(evRender, func(ev *termEvent) { ev.screen = s })
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "terminal.speak_start":
		t.event(evSpeakStart, func(ev *termEvent) { ev.c = &cmd })
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "terminal.speak_chunk":
		t.event(evSpeakChunk, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.speak_end":
		t.event(evSpeakEnd, nil)
	case "terminal.filler":
		t.event(evFiller, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.preview_voice":
		t.event(evPreview, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.stop_audio":
		t.event(evStopAudio, nil)
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "terminal.chime":
		t.event(evChime, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.metronome":
		spec, err := audio.NewMetronomeSpec(cmd.BPM, cmd.Beats, cmd.DurationS)
		if err != nil {
			c.fail(cmd.ID, err)
			return
		}
		id := cmd.StreamID
		if id == 0 {
			id = e.nextStream()
		}
		t.event(evMetronome, func(ev *termEvent) { ev.streamID, ev.spec = id, spec })
		c.reply(cmd.ID, map[string]any{"ok": true, "stream_id": id})
	case "terminal.play_tune":
		spec := audio.TuneSpec{Tempo: cmd.Tempo, Style: cmd.Style, Notes: cmd.Notes}
		pcm, err := audio.SynthesizeTune(spec)
		if err != nil {
			c.fail(cmd.ID, err)
			return
		}
		id := cmd.StreamID
		if id == 0 {
			id = e.nextStream()
		}
		t.event(evPlayTune, func(ev *termEvent) { ev.streamID, ev.data = id, pcm })
		c.reply(cmd.ID, map[string]any{"ok": true, "stream_id": id})
	case "terminal.power":
		t.event(evPower, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.volume":
		t.event(evVolume, func(ev *termEvent) { ev.c = &cmd })
	case "terminal.rec_start":
		t.event(evRecBegin, func(ev *termEvent) { ev.c = &cmd })
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "terminal.rec_cancel":
		t.event(evRecCancel, nil)
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "terminal.hits":
		reply := make(chan any, 1)
		t.event(evHits, func(ev *termEvent) { ev.reply = reply })
		select {
		case v := <-reply:
			if m, ok := v.(map[string]any); ok {
				c.reply(cmd.ID, m)
				return
			}
			c.fail(cmd.ID, errors.New("hits unavailable"))
		case <-time.After(2 * time.Second):
			c.fail(cmd.ID, errors.New("hits timeout"))
		}
	case "capture.start":
		if cmd.OutPath == "" || cmd.Seconds <= 0 {
			c.fail(cmd.ID, errors.New("capture.start needs out_path and seconds"))
			return
		}
		t.event(evCaptureStart, func(ev *termEvent) { ev.c = &cmd })
		c.reply(cmd.ID, map[string]any{"ok": true})
	case "capture.stop":
		t.event(evCaptureStop, nil)
		c.reply(cmd.ID, map[string]any{"ok": true})
	default:
		c.fail(cmd.ID, fmt.Errorf("unknown cmd %q", cmd.Cmd))
	}
}

func (e *Edge) synthesizeWAV(voiceID, text, outPath string) (string, error) {
	if text == "" || outPath == "" {
		return "", errors.New("tts.wav needs text and out_path")
	}
	tts := e.models.synthesizer()
	if tts == nil {
		return "", errors.New("TTS unavailable")
	}
	voice := audio.ByID(voiceID)
	if voice.ID == "" {
		voice = audio.ByID(audio.HeartVoice)
	}
	raw, rate := tts.Generate(text, voice, func() bool { return true })
	pcm, err := audio.SynthPCM(raw, rate)
	if err != nil {
		return "", err
	}
	if err := writeWAVAtomic(outPath, pcm); err != nil {
		return "", err
	}
	return outPath, nil
}

func (e *Edge) transcribeFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("path required")
	}
	pcm, err := audio.ReadWAVPCM(path)
	if err != nil {
		return "", err
	}
	stt := e.models.transcriber()
	if stt == nil {
		return "", errNoSTT
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return stt.Transcribe(ctx, pcm)
}

func normalizePortraitDir(dir string) (int, error) {
	if dir == "" {
		return 0, errors.New("dir required")
	}
	poses, err := pixel.LoadOrNormalizePoses(dir, pixel.PortraitHeight)
	if err != nil {
		return 0, err
	}
	seen := make(map[uint32]struct{})
	for _, img := range poses {
		if img == nil {
			continue
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := img.RGBAAt(x, y)
				seen[uint32(c.R)<<16|uint32(c.G)<<8|uint32(c.B)] = struct{}{}
			}
		}
	}
	return len(seen), nil
}
