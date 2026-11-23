package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const ttsWait = 30 * time.Second

var errTTS = errors.New("TTS stream failed")

type ttsRequest struct {
	ctx         context.Context
	voice, text string
	done        chan ttsResult
	queuedAt    time.Time
}

type ttsResult struct {
	pcm []byte
	err error
}

type synthWorker struct {
	ready    atomic.Bool
	requests chan ttsRequest
	warming  chan ttsRequest
	done     chan struct{}
}

type speechEngine interface {
	generate(text string, voice voiceChoice, keepGoing func() bool) ([]float32, int)
	close()
}

func newSynthWorker(ctx context.Context, open func() (speechEngine, error)) *synthWorker {
	w := &synthWorker{requests: make(chan ttsRequest), warming: make(chan ttsRequest), done: make(chan struct{})}
	go w.run(ctx, open)
	return w
}

func synthesizePCM(ctx context.Context, engine speechEngine, id, text string) ([]byte, error) {
	pcm, _, err := synthesizePCMWithScratch(ctx, engine, id, text, nil)
	return pcm, err
}

func synthesizePCMWithScratch(ctx context.Context, engine speechEngine, id, text string, scratch []float32) ([]byte, []float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, scratch, err
	}
	if id == "" {
		id = heartVoice
	}
	voice := voiceByID(id)
	if voice.ID == "" {
		return nil, scratch, fmt.Errorf("unknown voice %q", id)
	}
	samples, rate := engine.generate(text, voice, func() bool { return ctx.Err() == nil })
	if err := ctx.Err(); err != nil {
		return nil, scratch, err
	}
	resampleStart := time.Now()
	pcm, scratch, err := synthPCMWithScratch(samples, rate, scratch)
	traceFrom(ctx).measure(diagResample, time.Since(resampleStart))
	return pcm, scratch, err
}

func (w *synthWorker) run(ctx context.Context, open func() (speechEngine, error)) {
	defer close(w.done)
	engine, err := open()
	if err != nil {
		log.Printf("Lark TTS unavailable: %v", err)
		return
	}
	defer engine.close()
	w.ready.Store(true)
	defer w.ready.Store(false)
	scratch := make([]float32, 0, kokoroRate*10)
	for ctx.Err() == nil {
		req, ok := w.next(ctx)
		if !ok {
			return
		}
		if err := req.ctx.Err(); err != nil {
			req.done <- ttsResult{err: err}
			continue
		}
		turn, cancel := context.WithTimeout(req.ctx, ttsWait)
		stop := context.AfterFunc(ctx, cancel)
		trace := traceFrom(req.ctx)
		admitted := time.Now()
		trace.measure(diagTTSQueue, admitted.Sub(req.queuedAt))
		var pcm []byte
		pcm, scratch, err = synthesizePCMWithScratch(turn, engine, req.voice, req.text, scratch)
		trace.measure(diagTTSInfer, time.Since(admitted))
		stop()
		cancel()
		req.done <- ttsResult{pcm, err}
	}
}

func (w *synthWorker) next(ctx context.Context) (ttsRequest, bool) {
	// Let an active reply start as soon as the current inference ends instead
	// of waiting behind speculative intercom introductions.
	select {
	case req := <-w.requests:
		return req, true
	default:
	}
	select {
	case <-ctx.Done():
		return ttsRequest{}, false
	case req := <-w.requests:
		return req, true
	case req := <-w.warming:
		return req, true
	}
}

func (w *synthWorker) synthesize(ctx context.Context, voice, text string) ([]byte, error) {
	return w.synthesizeQueued(ctx, voice, text, time.Now())
}

func (w *synthWorker) synthesizeQueued(ctx context.Context, voice, text string, at time.Time) ([]byte, error) {
	return w.synthesizeOn(ctx, w.requests, voice, text, at)
}

func (w *synthWorker) warm(ctx context.Context, voice, text string) ([]byte, error) {
	return w.synthesizeOn(ctx, w.warming, voice, text, time.Now())
}

func (w *synthWorker) synthesizeOn(ctx context.Context, queue chan<- ttsRequest, voice, text string, at time.Time) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := ttsRequest{ctx: ctx, voice: voice, text: text, done: make(chan ttsResult, 1), queuedAt: at}
	select {
	case queue <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, errTTS
	}
	select {
	case res := <-req.done:
		return res.pcm, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Synthesized clips are cached by text until their bytes exceed the budget.
// Fillers and openers are pinned: they are the clips a turn reaches for first.
const clipCacheBytes = 24 << 20

type speechClips struct {
	mu    sync.Mutex
	clips map[clipKey]*clip
	order []clipKey
	bytes int
}

type clipKey struct{ voice, text string }

type clip struct {
	done chan struct{}
	pcm  []byte
	err  error
}

var thinkingFillers = []string{
	"Let me check.",
	"One moment.",
	"Hang on.",
	"Let me see.",
}

var commonOpeners = []string{
	"Sure!",
	"Okay!",
	"Hmm,",
	"Good question!",
	"Got it!",
	"Of course!",
}

func (c *speechClips) peek(voice, text string) []byte {
	c.mu.Lock()
	item := c.clips[clipKey{voice, text}]
	c.mu.Unlock()
	if item != nil {
		select {
		case <-item.done:
			if item.err == nil {
				return item.pcm
			}
		default:
		}
	}
	return nil
}

// await returns the PCM of an existing or in-flight clip. Unlike peek it waits
// for a speculative synthesis to finish, so the first chunk is never queued
// twice.
func (c *speechClips) await(ctx context.Context, voice, text string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	item := c.clips[clipKey{voice, text}]
	c.mu.Unlock()
	if item == nil {
		return nil, false
	}
	select {
	case <-item.done:
		if item.err == nil {
			return item.pcm, true
		}
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

func (c *speechClips) peekAny(voice string, phrases []string, seed uint64) []byte {
	if c == nil || len(phrases) == 0 {
		return nil
	}
	start := int(seed % uint64(len(phrases)))
	for i := range phrases {
		if pcm := c.peek(voice, phrases[(start+i)%len(phrases)]); len(pcm) > 0 {
			return pcm
		}
	}
	return nil
}

func (c *speechClips) warmFillers(ctx context.Context, w *synthWorker, voice string) {
	if c == nil || w == nil || voice == "" {
		return
	}
	for _, phrase := range thinkingFillers {
		if ctx.Err() != nil {
			return
		}
		_, _ = c.getWith(ctx, w.warm, voice, phrase)
	}
	for _, phrase := range commonOpeners {
		if ctx.Err() != nil {
			return
		}
		_, _ = c.getWith(ctx, w.warm, voice, phrase)
	}
}

func (c *speechClips) get(ctx context.Context, w *synthWorker, voice, text string) ([]byte, error) {
	return c.getWith(ctx, w.synthesize, voice, text)
}

func (c *speechClips) getWith(ctx context.Context, synth func(context.Context, string, string) ([]byte, error), voice, text string) ([]byte, error) {
	item, owner := c.reserve(voice, text)
	if !owner {
		select {
		case <-item.done:
			return item.pcm, item.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	item.pcm, item.err = synth(ctx, voice, text)
	c.complete(voice, text, item)
	return item.pcm, item.err
}

// reserve registers a clip so a concurrent turn can await it without starting
// a second synthesis. The caller owns the item only when owner is true.
func (c *speechClips) reserve(voice, text string) (item *clip, owner bool) {
	key := clipKey{voice, text}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clips == nil {
		c.clips = make(map[clipKey]*clip)
	}
	if existing := c.clips[key]; existing != nil {
		return existing, false
	}
	item = &clip{done: make(chan struct{})}
	c.clips[key] = item
	c.order = append(c.order, key)
	return item, true
}

func pinnedClip(text string) bool {
	return slices.Contains(thinkingFillers, text) || slices.Contains(commonOpeners, text)
}

// evictLocked drops least-recently reserved finished clips until the byte
// budget holds. Pinned phrases and in-flight clips stay.
func (c *speechClips) evictLocked() {
	remaining := c.order[:0]
	for _, key := range c.order {
		item := c.clips[key]
		if c.bytes <= clipCacheBytes || item == nil || pinnedClip(key.text) {
			if item != nil {
				remaining = append(remaining, key)
			}
			continue
		}
		select {
		case <-item.done:
			delete(c.clips, key)
			c.bytes -= len(item.pcm)
		default:
			remaining = append(remaining, key)
		}
	}
	c.order = remaining
}

// fill synthesizes a reserved clip and publishes it to awaiters.
func (c *speechClips) fill(ctx context.Context, w *synthWorker, voice, text string, item *clip) {
	item.pcm, item.err = w.warm(ctx, voice, text)
	c.complete(voice, text, item)
}

func (c *speechClips) complete(voice, text string, item *clip) {
	key := clipKey{voice, text}
	c.mu.Lock()
	if c.clips[key] == item {
		if item.err != nil {
			delete(c.clips, key)
			for i, ordered := range c.order {
				if ordered == key {
					c.order = append(c.order[:i], c.order[i+1:]...)
					break
				}
			}
		} else if item.pcm != nil {
			c.bytes += len(item.pcm)
			c.evictLocked()
		}
	}
	c.mu.Unlock()
	close(item.done)
}

type AudioStream struct {
	cond    *sync.Cond
	chunks  [][]byte
	firstAt time.Time
	size    int
	done    bool
	err     error
	evict   func()
}

func newAudioStream() *AudioStream { return &AudioStream{cond: sync.NewCond(&sync.Mutex{})} }

func (s *AudioStream) finish(err error) {
	s.cond.L.Lock()
	evict := s.finishLocked(err)
	s.cond.L.Unlock()
	if evict != nil {
		evict()
	}
}

func (s *AudioStream) finishLocked(err error) func() {
	if s.done {
		return nil
	}
	s.done, s.err = true, err
	s.cond.Broadcast()
	if err != nil {
		s.chunks = nil
		return s.evict
	}
	return nil
}

func (s *AudioStream) append(pcm []byte) bool {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if s.done || len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > maxPCM-s.size {
		return false
	}
	if len(s.chunks) == 0 {
		s.firstAt = time.Now()
	}
	s.chunks = append(s.chunks, pcm)
	s.size += len(pcm)
	s.cond.Broadcast()
	return true
}

// speechPause is shared zero PCM inserted between sentences.
var speechPause = make([]byte, sampleRate/5)

func (s *AudioStream) appendSilence(n int) bool {
	if n <= 0 {
		return true
	}
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if s.done || n > maxPCM-s.size {
		return false
	}
	if len(s.chunks) == 0 {
		s.firstAt = time.Now()
	}
	chunk := speechPause
	if n > len(chunk) {
		chunk = make([]byte, n)
	}
	s.chunks = append(s.chunks, chunk[:n])
	s.size += n
	s.cond.Broadcast()
	return true
}

func (s *AudioStream) traceFirst(t *turnTrace, start time.Time) {
	if t == nil || start.IsZero() {
		return
	}
	s.cond.L.Lock()
	at := s.firstAt
	s.cond.L.Unlock()
	if !at.IsZero() {
		t.measure(diagTTSFirst, at.Sub(start))
	}
}
func (s *AudioStream) chunk(ctx context.Context, i int) ([]byte, error) {
	return s.waitChunk(ctx, i, ttsWait)
}

func (s *AudioStream) waitChunk(ctx context.Context, i int, wait time.Duration) ([]byte, error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if i >= len(s.chunks) && !s.done {
		timeout := time.AfterFunc(wait, func() { s.finish(errors.New("TTS stream timed out")) })
		defer timeout.Stop()
		wake := context.AfterFunc(ctx, func() {
			s.cond.L.Lock()
			s.cond.Broadcast()
			s.cond.L.Unlock()
		})
		defer wake()
		for i >= len(s.chunks) && !s.done && ctx.Err() == nil {
			s.cond.Wait()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if i < len(s.chunks) {
		return s.chunks[i], nil
	}
	return nil, ctx.Err()
}

type audioEntry struct {
	id     string
	stream *AudioStream
}

type audioCache struct {
	mu    sync.Mutex
	order list.List
	items map[string]*list.Element
}

func (c *audioCache) add(s *AudioStream) (id string, size int, streaming bool) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if s.err != nil || s.size == 0 {
		return "", 0, false
	}
	key := make([]byte, 18)
	_, _ = rand.Read(key)
	id = base64.RawURLEncoding.EncodeToString(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]*list.Element, 16)
	}
	c.items[id] = c.order.PushFront(audioEntry{id, s})
	s.evict = func() {
		c.mu.Lock()
		if e := c.items[id]; e != nil {
			c.order.Remove(e)
			delete(c.items, id)
		}
		c.mu.Unlock()
	}
	if c.order.Len() > 16 {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(audioEntry).id)
	}
	if s.done {
		return id, s.size, false
	}
	return id, 0, true
}

func (c *audioCache) ServeAudio(w http.ResponseWriter, r *http.Request, id string) error {
	c.mu.Lock()
	e := c.items[id]
	if e != nil {
		c.order.MoveToFront(e)
	}
	c.mu.Unlock()
	if e == nil {
		return problem(404, "Audio not found")
	}
	s := e.Value.(audioEntry).stream
	s.cond.L.Lock()
	err, done, size, chunks := s.err, s.done, s.size, s.chunks
	s.cond.L.Unlock()
	if err != nil {
		return problem(404, "Audio not found")
	}
	w.Header().Set("Content-Type", pcmType)
	if done {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		for _, chunk := range chunks {
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		return nil
	}
	flush := http.NewResponseController(w)
	w.WriteHeader(200)
	if flush.Flush() != nil {
		return nil
	}
	for i := 0; ; i++ {
		chunk, err := s.chunk(r.Context(), i)
		if err != nil {
			// Omit the terminating chunk; the firmware must see a failed transfer.
			panic(http.ErrAbortHandler)
		}
		if chunk == nil {
			return nil
		}
		if _, err := w.Write(chunk); err != nil || flush.Flush() != nil {
			return nil
		}
	}
}

func spokenText(text string) (string, error) {
	text = plainText(text)
	if runeLen(text) > maxText || !strings.ContainsFunc(text, alphaNum) {
		return "", problem(400, fmt.Sprintf("Expected spoken text of 1–%d characters", maxText))
	}
	return text, nil
}

type speechItem struct {
	text string
	pcm  []byte
}

type speechTurn struct {
	stream      *AudioStream
	cancel      context.CancelFunc
	queue       chan speechItem
	sent        []string
	length      int
	firstQueued time.Time // Published to the worker by the first queue send.
}

func splitCachedOpener(clips *speechClips, voice, text string) (pcm []byte, rest string, ok bool) {
	if clips == nil || text == "" {
		return nil, text, false
	}
	for _, opener := range commonOpeners {
		if text == opener || strings.HasPrefix(text, opener+" ") {
			if cached := clips.peek(voice, opener); len(cached) > 0 {
				return cached, strings.TrimSpace(text[len(opener):]), true
			}
		}
	}
	return nil, text, false
}

func newSpeechTurn(ctx context.Context, cancel context.CancelFunc, w *synthWorker, clips *speechClips, voice string, onFirst func(*AudioStream)) *speechTurn {
	t := &speechTurn{stream: newAudioStream(), cancel: cancel, queue: make(chan speechItem, maxText)}
	go func() {
		defer cancel()
		first := true
		emitPCM := func(pcm []byte) bool {
			if !first && !t.stream.appendSilence(sampleRate/5) {
				t.stream.finish(errTTS)
				return false
			}
			if !t.stream.append(pcm) {
				t.stream.finish(errTTS)
				return false
			}
			if first && onFirst != nil {
				onFirst(t.stream)
			}
			first = false
			return true
		}
		for item := range t.queue {
			if len(item.pcm) > 0 {
				if !emitPCM(item.pcm) {
					return
				}
				continue
			}
			text := item.text
			if clips != nil {
				if pcm, ok := clips.await(ctx, voice, text); ok {
					if !emitPCM(pcm) {
						return
					}
					continue
				}
			}
			if first {
				if openerPCM, rest, ok := splitCachedOpener(clips, voice, text); ok {
					if !emitPCM(openerPCM) {
						return
					}
					if rest == "" {
						continue
					}
					text = rest
				}
			}
			at := t.firstQueued
			if !first {
				at = time.Now()
			}
			pcm, err := w.synthesizeQueued(ctx, voice, text, at)
			if err != nil {
				t.stream.finish(err)
				return
			}
			if !emitPCM(pcm) {
				return
			}
		}
		t.stream.finish(nil)
	}()
	return t
}

func (t *speechTurn) send(s string) {
	// At most maxText printable characters can be accepted, so send never waits on TTS.
	n := runeLen(s)
	if n == 0 || t.length+n > maxText {
		return
	}
	if t.length == 0 {
		t.firstQueued = time.Now()
	}
	t.length += n
	t.sent = append(t.sent, s)
	t.queue <- speechItem{text: s}
}

func (t *speechTurn) sendPCM(pcm []byte) {
	if len(pcm) == 0 {
		return
	}
	t.queue <- speechItem{pcm: pcm}
}

func (t *speechTurn) discard() {
	t.stream.finish(context.Canceled)
	t.cancel()
	close(t.queue)
}

func (t *speechTurn) complete(final []string) bool {
	return t.completeAfter(0, final)
}

func (t *speechTurn) completeAfter(offset int, final []string) bool {
	sent := t.sent[min(offset, len(t.sent)):]
	if len(sent) > len(final) {
		return false
	}
	for i, s := range sent {
		if s != final[i] {
			return false
		}
	}
	for _, s := range final[len(sent):] {
		t.send(s)
	}
	close(t.queue)
	return true
}
