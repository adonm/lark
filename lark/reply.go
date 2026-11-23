package main

import (
	"context"
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
	text       string
	audioLen   int
	started    time.Time
	emitMark   uint64 // emit sequence before the speculative model ran
	cancel     context.CancelFunc
	fillCtx    context.Context // per-spec context for speculative TTS fills
	cancelFill context.CancelFunc

	mu        sync.Mutex
	promoted  bool
	aborted   bool
	warming   bool
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
		text:       text,
		audioLen:   audioLen,
		started:    time.Now(),
		emitMark:   b.emits.mark(companionID),
		cancel:     cancel,
		fillCtx:    fillCtx,
		cancelFill: cancelFill,
		done:       make(chan specResult, 1),
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
		prepare := func() (string, string) {
			companion := b.companionFor(companionID)
			return companion.Name, b.turnPromptWithState(ctx, companionID, source, text, companion, ch, time.Now().In(b.location()))
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
		if err != nil || b.worker == nil || !b.worker.ready.Load() {
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
	prepare := func() (string, string) {
		companion := b.companionFor(sender.ID)
		return companion.Name, b.turnPromptWithState(ctx, sender.ID, options.source, text, companion, options.channel, time.Now().In(b.location()))
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
	if !options.textOnly && speechErr == nil && (eager != nil || b.worker != nil && b.worker.ready.Load()) {
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
