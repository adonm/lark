package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type delivery struct {
	sent, delivered, failed []string
}

type generatedReply struct {
	reply    string
	sid      string
	audio    *AudioStream
	ttsStart time.Time
	routed   bool // the turn called emit, so its reply already went somewhere explicit
}

// turnReply is the Delta Chat message a device read out. A spoken reply that
// follows it goes back to the same chat unless the agent emits somewhere else.
type turnReply struct {
	name        string // sender name shown in the prompt
	companionID string // companion whose Delta Chat account received the message
	chatID      uint32 // chat to reply into, so groups and renames keep working
}

type turnChannel struct {
	channel    string // "in_person", "deltachat", "timer"
	sender     string
	senderKind string // "contact", "companion"
	reply      *turnReply
	private    bool
	spec       *specTurn
}

type turnOptions struct {
	source   string
	channel  turnChannel
	publish  func(*AudioStream, time.Time)
	spec     *specTurn
	textOnly bool // skip TTS (Delta Chat text replies)
}

type specTurn struct {
	text        string
	companionID string
	audioLen    int
	started     time.Time
	emitMark    uint64 // emit sequence before the speculative model ran
	cancel      context.CancelFunc
	fillCtx     context.Context // per-spec context for speculative TTS fills
	cancelFill  context.CancelFunc
	gate        *specGate // nil outside a terminal hub

	mu        sync.Mutex
	promoted  bool
	aborted   bool
	warming   bool
	held      []*heldMCP
	firstAt   time.Time
	onSent    func(string)
	onTool    func()
	onChat    func(chatEntry)
	sentences []string
	entries   []chatEntry
	usedTool  bool
	done      chan specResult
}

type specResult struct {
	reply string
	sid   string
	err   error
}

// heldMCP is a mutating tool call from a speculative turn. Its HTTP handler
// waits here until the turn is promoted or aborted.
type heldMCP struct {
	run  func() (object, error)
	done chan struct{}
	out  object
	err  error
}

var errSpecAborted = errors.New("speculative turn was cancelled; the change was not applied")

// specGate tracks speculations that are still in flight. MCP requests carry no
// turn identity (OpenCode calls Lark over HTTP), so a mutating call is
// attributed to the only active speculation, or to the one matching its
// companion argument.
type specGate struct {
	mu     sync.Mutex
	active map[*specTurn]struct{}
}

func (g *specGate) add(s *specTurn) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.active == nil {
		g.active = make(map[*specTurn]struct{})
	}
	g.active[s] = struct{}{}
	g.mu.Unlock()
}

func (g *specGate) remove(s *specTurn) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.active, s)
	g.mu.Unlock()
}

// find returns the speculation a tool call belongs to. Explicit companion
// arguments disambiguate concurrent turns; otherwise a single in-flight
// speculation owns the call.
func (g *specGate) find(args object) *specTurn {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.active) == 0 {
		return nil
	}
	ids := func(candidate *specTurn) bool {
		for _, key := range []string{"companion_id", "to", "from", "from_companion_id", "to_companion_id"} {
			if v := strings.ToLower(strings.TrimSpace(str(args[key]))); v != "" && v != "all" {
				return strings.EqualFold(v, candidate.companionID)
			}
		}
		return false
	}
	for s := range g.active {
		if ids(s) {
			return s
		}
	}
	if len(g.active) == 1 {
		for s := range g.active {
			return s
		}
	}
	return nil
}

func (s *specTurn) hold(ctx context.Context, run func() (object, error)) (object, error) {
	s.mu.Lock()
	if s.promoted {
		s.mu.Unlock()
		return run()
	}
	if s.aborted {
		s.mu.Unlock()
		return nil, errSpecAborted
	}
	call := &heldMCP{run: run, done: make(chan struct{})}
	s.held = append(s.held, call)
	s.mu.Unlock()
	select {
	case <-call.done:
		return call.out, call.err
	case <-ctx.Done():
		// The caller gave up; do not apply it on promotion.
		s.mu.Lock()
		for i, held := range s.held {
			if held == call {
				s.held = append(s.held[:i], s.held[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

// releaseHeld applies the deferred calls in the order the model issued them.
func (s *specTurn) releaseHeld() {
	s.mu.Lock()
	held := s.held
	s.held = nil
	s.mu.Unlock()
	for _, call := range held {
		call.out, call.err = call.run()
		close(call.done)
	}
}

func (s *specTurn) rejectHeld() {
	s.mu.Lock()
	held := s.held
	s.held = nil
	s.mu.Unlock()
	for _, call := range held {
		call.err = errSpecAborted
		close(call.done)
	}
}

func normalizeSpecText(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimRight(s, ".!?,;:")
}

func (b *larkServer) startSpeculate(companionID, text string, audioLen int, source string, ch turnChannel) *specTurn {
	if b == nil || b.oc == nil || companionID == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	if _, direct, err := b.intercomIntent(companionID, text); err != nil || direct != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(b.root, turnTimeout)
	fillCtx, cancelFill := context.WithTimeout(b.root, turnTimeout)
	s := &specTurn{
		text:        text,
		companionID: strings.ToLower(companionID),
		audioLen:    audioLen,
		started:     time.Now(),
		emitMark:    b.emits.mark(companionID),
		cancel:      cancel,
		fillCtx:     fillCtx,
		cancelFill:  cancelFill,
		done:        make(chan specResult, 1),
	}
	if b.hub != nil {
		s.gate = b.hub.gate
		s.gate.add(s)
	}
	// Pre-synthesize the first sentence while the turn is still speculative so a
	// promoted reply can publish PCM immediately instead of waiting for TTS.
	voice := b.voiceFor(companionID)
	warm := func(sent string) {
		if b.worker == nil || !b.worker.ready.Load() {
			return
		}
		clean, err := spokenText(sent)
		if err != nil || strings.TrimSpace(clean) == "" {
			return
		}
		s.mu.Lock()
		if s.aborted || s.warming {
			s.mu.Unlock()
			return
		}
		s.warming = true
		s.mu.Unlock()
		// Reserve synchronously so a promoted turn awaits this clip instead of
		// queueing a duplicate synthesis for the same first chunk. The fill runs
		// on the spec's own context so aborting the speculation drops it.
		item, owner := b.clips.reserve(voice, clean)
		if !owner {
			return
		}
		go b.clips.fill(s.fillCtx, b.worker, voice, clean, item)
	}
	go func() {
		defer cancel()
		stream := turnStream{
			firstChunk: true,
			sentence: func(sent string) {
				s.mu.Lock()
				first := s.firstAt.IsZero()
				if first {
					s.firstAt = time.Now()
				}
				s.mu.Unlock()
				if first {
					b.diag.specFirst(time.Since(s.started))
				}
				warm(sent)
				s.mu.Lock()
				if s.aborted {
					s.mu.Unlock()
					return
				}
				if s.promoted && s.onSent != nil {
					fn := s.onSent
					s.mu.Unlock()
					fn(sent)
					return
				}
				s.sentences = append(s.sentences, sent)
				s.mu.Unlock()
			},
			tool: func() {
				s.mu.Lock()
				if s.aborted {
					s.mu.Unlock()
					return
				}
				if s.promoted && s.onTool != nil {
					fn := s.onTool
					s.mu.Unlock()
					fn()
					return
				}
				s.usedTool = true
				s.mu.Unlock()
			},
		}
		onChat := func(entry chatEntry) {
			s.mu.Lock()
			if s.aborted {
				s.mu.Unlock()
				return
			}
			if s.promoted && s.onChat != nil {
				fn := s.onChat
				s.mu.Unlock()
				fn(entry)
				return
			}
			s.entries = append(s.entries, entry)
			s.mu.Unlock()
		}
		prepare := func() (companionState, string) {
			companion := b.companionFor(companionID)
			return companion, b.turnPromptWithState(ctx, companionID, source, text, companion, ch, time.Now().In(b.location()))
		}
		reply, sid, err := b.oc.askPrepared(ctx, companionID, prepare, stream, source, nil, onChat)
		s.done <- specResult{reply: reply, sid: sid, err: err}
	}()
	return s
}

func (s *specTurn) abort() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.aborted = true
	s.onSent, s.onTool, s.onChat = nil, nil, nil
	s.mu.Unlock()
	s.gate.remove(s)
	s.rejectHeld()
	s.cancelFill()
	s.cancel()
}

func (s *specTurn) isPromoted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promoted
}

func (s *specTurn) promote(ctx context.Context, onSent func(string), onTool func(), onChat func(chatEntry)) specResult {
	s.mu.Lock()
	if s.aborted {
		s.mu.Unlock()
		return specResult{err: context.Canceled}
	}
	s.promoted = true
	s.onSent = onSent
	s.onTool = onTool
	s.onChat = onChat
	entries := s.entries
	s.entries = nil
	usedTool := s.usedTool
	sentences := s.sentences
	s.sentences = nil
	s.mu.Unlock()
	s.gate.remove(s)
	// Tool side effects must land before the reply that describes them.
	s.releaseHeld()

	if onChat != nil {
		for _, entry := range entries {
			onChat(entry)
		}
	}
	if usedTool && onTool != nil {
		onTool()
	}
	if onSent != nil {
		for _, sent := range sentences {
			onSent(sent)
		}
	}
	select {
	case res := <-s.done:
		return res
	case <-ctx.Done():
		s.abort()
		return specResult{err: ctx.Err()}
	}
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
		return newSpeechTurn(audioCtx, cancel, b.worker, &b.clips, b.voiceFor(sender.ID), onFirst)
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
		if options.textOnly || isSilentReply(s) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "[silent") {
			return
		}
		start := time.Now()
		clean, err := spokenText(s)
		if err != nil || b.worker == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		spoken = true
		ensureEager(start)
		eager.send(clean)
	}
	pieces := turnStream{firstChunk: true, sentence: onSentence, tool: func() {
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
	prepare := func() (companionState, string) {
		companion := b.companionFor(sender.ID)
		return companion, b.turnPromptWithState(ctx, sender.ID, options.source, text, companion, options.channel, time.Now().In(b.location()))
	}
	onChat := func(entry chatEntry) {
		if entry.Role == "assistant" {
			entry.Sender = b.companionFor(sender.ID).Name
		}
		b.chat(sender.ID, entry, true)
	}
	if options.textOnly {
		onChat = nil // The caller records its own final line.
	}
	emitBase := b.emits.mark(sender.ID)
	var raw, sid string
	var err error
	if options.spec != nil {
		emitBase = options.spec.emitMark
		res := options.spec.promote(ctx, onSentence, pieces.tool, onChat)
		raw, sid, err = res.reply, res.sid, res.err
	} else {
		raw, sid, err = b.oc.askPrepared(ctx, sender.ID, prepare, pieces, options.source, nil, onChat)
	}
	mu.Lock()
	defer mu.Unlock()
	if err != nil {
		traceFrom(ctx).fail()
		drop(true)
		return generatedReply{}, err
	}
	reply := strings.TrimSpace(fenced.ReplaceAllString(raw, ""))
	routed := b.emits.routed(emitBase, sender.ID)
	if isSilentReply(reply) {
		traceFrom(ctx).silent()
		drop(true)
		return generatedReply{sid: sid, routed: routed}, nil
	}
	var stream *AudioStream
	clean, speechErr := spokenText(reply)
	if !options.textOnly && speechErr == nil && (eager != nil || b.worker != nil && b.worker.available()) {
		ex := sentenceExtractor{firstChunk: true}
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
	return generatedReply{reply: reply, sid: sid, audio: stream, ttsStart: ttsStart, routed: routed}, nil
}
