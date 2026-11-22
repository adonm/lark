package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpenCode(t *testing.T, ctx context.Context, dir string, handle http.HandlerFunc) *openCode {
	t.Helper()
	return testOpenCodeOnDB(t, ctx, dir, testDB(t, ":memory:"), handle)
}

func testOpenCodeOnDB(t *testing.T, ctx context.Context, dir string, db *larkDB, handle http.HandlerFunc) *openCode {
	t.Helper()
	api := httptest.NewServer(handle)
	t.Cleanup(api.Close)
	config := filepath.Join(dir, "oc-api.json")
	if err := writeMap(config, object{"url": api.URL, "password": "unit"}); err != nil {
		t.Fatal(err)
	}
	return newOpenCode(ctx, upstream{"OpenCode", api.Client(), &fileCache[endpoint]{path: config, read: openCodeEndpoint}},
		db, dir, ocSettings{})
}

func expectValue[T comparable](t *testing.T, result <-chan T, want T) {
	t.Helper()
	select {
	case got := <-result:
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %v", want)
	}
}

func TestReconcileSessionUsesCurrentAgentModel(t *testing.T) {
	const device, sid = "lark-model-test", "ses-existing"
	models := []ocModel{{"a", "first", "low"}, {"b", "second", "small"}}
	changed := make(chan ocModel, len(models))
	current := make(chan ocModel, 1)
	current <- models[0]
	o := testOpenCode(t, t.Context(), t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/agent/lark":
			if r.URL.Query().Get("location[directory]") == "" {
				t.Error("model lookup must use the session location")
			}
			model := <-current
			current <- model
			sendJSON(w, 200, object{"data": object{"id": "lark", "model": model}})
		case "/api/session/" + sid:
			sendJSON(w, 200, object{"data": object{"id": sid, "agent": "lark", "model": ocModel{"old", "model", ""}}})
		case "/api/session/" + sid + "/model":
			var in struct{ Model ocModel }
			if json.NewDecoder(r.Body).Decode(&in) != nil {
				t.Error("invalid model update")
			}
			changed <- in.Model
			sendJSON(w, 200, object{})
		default:
			http.NotFound(w, r)
		}
	})
	o.settings.Agent = "lark"
	if err := o.save(device, sid); err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		<-current
		current <- model
		got, err := o.ensureSession(t.Context(), device, "Cat", true)
		if err != nil || got != sid {
			t.Fatalf("did not preserve session %q: %q, %v", sid, got, err)
		}
		expectValue(t, changed, model)
	}
}

func TestOpenCodeAdmitsPromptBeforeEventStreamConnects(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prompted := make(chan time.Time, 1)
	const sid = "ses-disconnected"
	o := testOpenCode(t, ctx, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/event":
			<-r.Context().Done()
		case "/api/session/" + sid + "/prompt":
			prompted <- time.Now()
			sendJSON(w, 200, object{"data": object{"id": "prompt"}})
		case "/api/experimental/session/" + sid + "/wait":
			sendJSON(w, 200, object{})
		case "/api/session/" + sid + "/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Ready?"}]},{"id":"prompt","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	start := time.Now()
	reply, err := o.runTurnStreaming(ctx, sid, "Hello", turnStream{sentence: func(string) {
		t.Error("disconnected stream spoke partial text")
	}}, "", nil)
	if err != nil || reply != "Ready?" {
		t.Fatalf("final reply %q, %v", reply, err)
	}
	if delay := (<-prompted).Sub(start); delay >= 400*time.Millisecond {
		t.Fatalf("prompt waited %s for a disconnected event stream", delay)
	}
}

func TestEagerReplyPublishesPCMBeforeSSEFinishes(t *testing.T) {
	for _, mode := range []string{"matching", "divergent", "tool"} {
		t.Run(mode, func(t *testing.T) {
			const id, sid = "lark-eager", "ses-eager"
			const prefix, tail = "Ready? ", "Here is another sentence."
			final := prefix + tail
			if mode == "divergent" {
				final = "A different first sentence. " + tail
			} else if mode == "tool" {
				final = tail
			}
			prompted, sendTool, finishSSE := make(chan struct{}), make(chan struct{}), make(chan struct{})
			b, base := wsHarness(t, func(b *larkServer, dir string) {
				b.worker = fakeTTS(b.root, nil)
				b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/event":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, ": connected\n\n")
						flusher := w.(http.Flusher)
						flusher.Flush()
						event := func(kind, text string) {
							fmt.Fprintf(w, "data: {\"type\":%q,\"data\":{\"sessionID\":%q,\"delta\":%q}}\n\n", kind, sid, text)
							flusher.Flush()
						}
						select {
						case <-prompted:
							event("session.text.delta", prefix)
						case <-r.Context().Done():
							return
						}
						if mode == "tool" {
							select {
							case <-sendTool:
								event("session.tool.input.started", "")
							case <-r.Context().Done():
								return
							}
						}
						select {
						case <-finishSSE:
							event("session.text.delta", tail)
							event("session.execution.succeeded", "")
						case <-r.Context().Done():
						}
					case "/api/session":
						fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
					case "/api/session/" + sid + "/prompt":
						_, _ = io.WriteString(w, `{"data":{"id":"prompt"}}`)
						close(prompted)
					case "/api/session/" + sid + "/message":
						select {
						case <-finishSSE:
							fmt.Fprintf(w, `{"data":[{"id":"answer","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"prompt","type":"user"}]}`, final)
						case <-r.Context().Done():
						}
					default:
						http.NotFound(w, r)
					}
				})
			})
			b.oc.feed.sig.wait(t, func() bool {
				b.oc.feed.mu.Lock()
				defer b.oc.feed.mu.Unlock()
				return b.oc.feed.connected
			})
			device := testConnectWS(t, base, "unit", false)
			device.hello(t, id, "Cake")
			b.ws.mu.Lock()
			b.ws.startTurn(b.ws.terminals[id], time.Now(), false, "typed", func(ctx context.Context, turn uint64) {
				b.ws.runTurn(ctx, b.ws.terminals[id], turn, "Speak two sentences.", "typed", nil)
			})
			b.ws.mu.Unlock()
			ttsMsg := func(op uint8) func(wsFrame) bool {
				return func(f wsFrame) bool {
					p := framePayload(f)
					return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == op
				}
			}
			ttsAudio := func(f wsFrame) uint64 { return binary.LittleEndian.Uint64(framePayload(f)[1:9]) }
			startAudio := ttsAudio(device.until(t, ttsMsg(ttsOpStart)))
			device.until(t, isPlayback) // The upstream is still blocked on finishSSE.
			b.diag.sig.wait(t, func() bool {
				turns := b.diagnosticReport().Turns
				return len(turns) != 0 && turns[0].TTS != nil && turns[0].Total == nil
			})
			if mode == "tool" {
				close(sendTool)
			}
			close(finishSSE)
			var endAudio uint64
			hasEnd := false
			continued := false
			for !hasEnd {
				f := device.read(t)
				switch {
				case isPlayback(f):
					continued = true
				case ttsMsg(ttsOpStop)(f):
					if mode != "divergent" || ttsAudio(f) != startAudio {
						t.Fatal("unexpected audio abort", f.data)
					}
				case ttsMsg(ttsOpStart)(f):
					if mode != "divergent" || ttsAudio(f) == startAudio {
						t.Fatal("unexpected audio generation", f.data)
					}
				case ttsMsg(ttsOpEnd)(f):
					endAudio = ttsAudio(f)
					hasEnd = true
				}
			}
			if !continued || (startAudio == endAudio) != (mode == "matching" || mode == "tool") {
				t.Fatalf("unexpected completion: continued=%t, start=%d, end=%d", continued, startAudio, endAudio)
			}
		})
	}
}

func TestWSEagerTTSFailureShowsAnswerUnlessInterrupted(t *testing.T) {
	for _, mode := range []string{"before-final", "after-final", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			const id, sid = "lark-eager", "ses-eager"
			const first, second = "This is the first sentence.", "Here is the answer."
			prompted, finish := make(chan struct{}), make(chan struct{})
			b, base := wsHarness(t, func(b *larkServer, dir string) {
				b.worker = &synthWorker{requests: make(chan ttsRequest), done: make(chan struct{})}
				b.worker.ready.Store(true)
				b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/event":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, ": connected\n\n")
						f := w.(http.Flusher)
						f.Flush()
						select {
						case <-prompted:
							fmt.Fprintf(w, "data: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":%q,\"delta\":%q}}\n\n", sid, first+" "+second+" ")
							f.Flush()
						case <-r.Context().Done():
							return
						}
						select {
						case <-finish:
							fmt.Fprintf(w, "data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":%q}}\n\n", sid)
							f.Flush()
							<-r.Context().Done()
						case <-r.Context().Done():
						}
					case "/api/session":
						fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
					case "/api/session/" + sid + "/prompt":
						_, _ = io.WriteString(w, `{"data":{"id":"prompt"}}`)
						close(prompted)
					case "/api/session/" + sid + "/message":
						fmt.Fprintf(w, `{"data":[{"id":"answer","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"prompt","type":"user"}]}`, first+" "+second)
					default:
						http.NotFound(w, r)
					}
				})
			})
			nextSpeech := func(want string) ttsRequest {
				t.Helper()
				select {
				case req := <-b.worker.requests:
					if req.text != want {
						t.Fatalf("requested speech %q, want %q", req.text, want)
					}
					return req
				case <-time.After(2 * time.Second):
					t.Fatalf("did not synthesize %q", want)
					return ttsRequest{}
				}
			}
			idle := func() bool {
				b.ws.mu.Lock()
				defer b.ws.mu.Unlock()
				d := b.ws.terminals[id]
				return !d.busy && d.stopAudio == nil && !d.awaitAck
			}
			board := testConnectWS(t, base, "unit", false)
			board.helloV2(t, id, "Cake")
			b.ws.mu.Lock()
			b.ws.startTurn(b.ws.terminals[id], time.Now(), false, "typed", func(ctx context.Context, turn uint64) {
				b.ws.runTurn(ctx, b.ws.terminals[id], turn, "Speak two sentences.", "typed", nil)
			})
			b.ws.mu.Unlock()
			nextSpeech(first).done <- ttsResult{pcm: make([]byte, 640)}
			ttsMatch := func(op uint8) func(wsFrame) bool {
				return func(f wsFrame) bool {
					p := framePayload(f)
					return hasProto(msgTTSCtrl, "")(f) && len(p) >= 9 && p[0] == op
				}
			}
			startFrame := board.until(t, ttsMatch(ttsOpStart))
			startAudio := binary.LittleEndian.Uint64(framePayload(startFrame)[1:9])
			board.until(t, isPlayback)
			b.diag.sig.wait(t, func() bool {
				report := b.diagnosticReport()
				return len(report.Turns) == 1 && report.Turns[0].FirstPCM != nil && report.Turns[0].Render != nil
			})
			row := b.diagnosticReport().Turns[0]
			if row.Gate == nil || row.FirstSentence == nil || row.First == nil ||
				*row.FirstSentence < *row.First || *row.FirstPCM < *row.FirstSentence || row.E2E != nil {
				t.Fatalf("invalid split timings for typed turn: %+v", row)
			}
			tail := nextSpeech(second)
			switch mode {
			case "before-final":
				tail.done <- ttsResult{err: errTTS}
				board.until(t, ttsMatch(ttsOpStop))
				close(finish)
			case "after-final":
				close(finish)
				b.ws.sig.wait(t, func() bool {
					b.ws.mu.Lock()
					defer b.ws.mu.Unlock()
					return !b.ws.terminals[id].busy
				})
				tail.done <- ttsResult{err: errTTS}
				board.until(t, ttsMatch(ttsOpStop))
			case "interrupted":
				board.sendProto(t, msgTTSAck, encodeTTSAck(true, startAudio))
				board.barrier(t)
				tail.done <- ttsResult{err: errTTS}
				close(finish)
				b.ws.sig.wait(t, idle)
				b.ws.mu.Lock()
				view, pending := b.ws.terminals[id].view, b.ws.terminals[id].pending
				b.ws.mu.Unlock()
				if view != nil || pending != nil {
					t.Fatal("interrupted speech displayed a failure card")
				}
				return
			}
			b.ws.sig.wait(t, func() bool {
				b.ws.mu.Lock()
				defer b.ws.mu.Unlock()
				d := b.ws.terminals[id]
				return d.scene.title == "Answer" && strings.Contains(d.svg.source, first) && strings.Contains(d.svg.source, second)
			})
		})
	}
}

func TestOpenCodeFinalReplyPaginatesToAdmittedPrompt(t *testing.T) {
	const sid = "ses-long-turn"
	const reply = "The authoritative reply."
	first := []object{{"id": "answer", "type": "assistant", "finish": "stop",
		"content": []object{{"type": "text", "text": reply}}}}
	for i := range 31 {
		first = append(first, object{"id": fmt.Sprintf("step-%d", i), "type": "assistant", "finish": "other"})
	}
	second := []object{
		{"id": "tool", "type": "assistant", "finish": "tool-calls"},
		{"id": "another-step", "type": "assistant", "finish": "other"},
		{"id": "prompt", "type": "user"},
		{"id": "previous-idle", "type": "idle", "outcome": "failed"},
	}
	const next = "older+/="
	o := testOpenCode(t, t.Context(), t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/" + sid + "/prompt":
			_, _ = io.WriteString(w, `{"data":{"id":"prompt"}}`)
		case "/api/experimental/session/" + sid + "/wait":
			w.WriteHeader(204)
		case "/api/session/" + sid + "/message":
			q := r.URL.Query()
			switch {
			case q.Get("limit") == "32" && q.Get("order") == "desc" && q.Get("cursor") == "":
				_ = json.NewEncoder(w).Encode(object{"data": first, "cursor": object{"next": next}})
			case q.Get("limit") == "32" && q.Get("order") == "" && q.Get("cursor") == next:
				_ = json.NewEncoder(w).Encode(object{"data": second, "cursor": object{"next": "ignore-earlier-turns"}})
			default:
				t.Errorf("unexpected page query %q", r.URL.RawQuery)
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
	tools := 0
	got, err := o.runTurnStreaming(t.Context(), sid, "Long turn", turnStream{tool: func() { tools++ }}, "", nil)
	if err != nil || got != reply || tools != 1 {
		t.Fatalf("reply %q, err %v, tool callbacks %d", got, err, tools)
	}
}

func TestOpenCodeCancelAndReset(t *testing.T) {
	for _, action := range []string{"cancel", "reset", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			const termID, id, sid = "vocat-0001", "cake", "ses-test"
			interrupts := make(chan string, 4)
			release := make(chan struct{})
			b, base := wsHarness(t, func(b *larkServer, dir string) {
				b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/session/"+sid+"/interrupt" {
						w.WriteHeader(404)
						return
					}
					interrupts <- r.URL.Path
					select {
					case <-release:
						_, _ = io.WriteString(w, `{"data":{"interrupted":false}}`)
					case <-r.Context().Done():
					}
				})
				if err := b.oc.save(id, sid); err != nil {
					t.Fatal(err)
				}
			})
			c := testConnectWS(t, base, "unit", false)
			c.hello(t, termID, "Cake")
			if action != "reset" {
				b.ws.mu.Lock()
				b.ws.terminals[termID].busy = true
				b.ws.mu.Unlock()
			}
			reset := make(chan error, 1)
			switch action {
			case "cancel":
				b.ws.mu.Lock()
				b.ws.cancelTurn(b.ws.terminals[termID], "stop")
				b.ws.mu.Unlock()
			case "reset":
				go func() {
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/companions/"+id+"/reset", nil)
					_, err := b.route(httptest.NewRecorder(), req, "/api/companions/"+id+"/reset")
					reset <- err
				}()
			case "disconnect":
				c.conn.Close()
			}
			expectValue(t, interrupts, "/api/session/"+sid+"/interrupt")
			if got, err := b.oc.get(id); err != nil || got != sid {
				t.Fatalf("stored session changed before the interrupt finished: %q, %v", got, err)
			}
			close(release)
			if action == "reset" {
				expectValue[error](t, reset, nil)
			}
			if action == "reset" {
				b.oc.sig.wait(t, func() bool { saved, _ := b.oc.get(id); return saved == "" })
			}
		})
	}
}

func TestOpenCodeInterruptOrdersReplacementPrompt(t *testing.T) {
	const id, sid = "lark-order", "ses-order"
	prompts := make(chan string, 4)
	interrupt := make(chan bool, 4)
	allowPrompt, allowStop := make(chan struct{}), make(chan struct{})
	root, stop := context.WithCancel(t.Context())
	defer stop()
	o := testOpenCode(t, root, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			raw, _ := io.ReadAll(r.Body)
			if got := testObject(t, raw)["title"]; got != "Lark: Cake ("+id+")" {
				t.Error("unexpected character title:", got)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
		case "/api/session/" + sid:
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
		case "/api/session/" + sid + "/prompt":
			raw, _ := io.ReadAll(r.Body)
			text := str(testObject(t, raw)["text"])
			prompts <- text
			if text == "First" {
				select {
				case <-allowPrompt:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
		case "/api/session/" + sid + "/interrupt":
			interrupt <- true
			select {
			case <-allowStop:
				_, _ = io.WriteString(w, `{"data":{"interrupted":true}}`)
			case <-r.Context().Done():
			}
		case "/api/experimental/session/" + sid + "/wait":
			w.WriteHeader(204)
		case "/api/session/" + sid + "/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg2","type":"assistant","finish":"stop","content":[{"type":"text","text":"Done"}]},{"id":"msg1","type":"user"}]}`)
		default:
			w.WriteHeader(404)
		}
	})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := o.askPrepared(root, id, func() (string, string) { return "Cake", "First" }, turnStream{}, "text", nil, nil)
		first <- err
	}()
	expectValue(t, prompts, "First")
	o.interruptDevice(id)
	go func() {
		_, _, err := o.askPrepared(root, id, func() (string, string) { return "Cake", "Second" }, turnStream{}, "text", nil, nil)
		second <- err
	}()
	select {
	case <-interrupt:
		t.Fatal("interrupt raced prompt admission")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowPrompt)
	expectValue(t, interrupt, true)
	select {
	case <-prompts:
		t.Fatal("replacement prompt outran interrupt")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowStop)
	expectValue(t, prompts, "Second")
	expectValue[error](t, second, nil)
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("first turn did not stop")
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not return")
	}
}

func TestOpenCodeFallbackTitlesAndChat(t *testing.T) {
	const reply = "Done.\n```lark-message\n{\"to\":\"Mint\",\"text\":\"Do not deliver\"}\n```"
	titles, prompts := make(chan string, 4), make(chan string, 4)
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session":
				raw, _ := io.ReadAll(r.Body)
				titles <- str(testObject(t, raw)["title"])
				_, _ = io.WriteString(w, `{"data":{"id":"ses-ask"}}`)
			case "/api/session/ses-ask/prompt":
				raw, _ := io.ReadAll(r.Body)
				prompts <- str(testObject(t, raw)["text"])
				_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
			case "/api/experimental/session/ses-ask/wait":
				w.WriteHeader(204)
			case "/api/session/ses-ask/message":
				fmt.Fprintf(w, `{"data":[{"id":"msg2","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"msg1","type":"user"}]}`, reply)
			default:
				w.WriteHeader(404)
			}
		})
	})
	for _, item := range []struct{ id, name string }{{"lark-empty", ""}, {"lark-equal", "lark-equal"}} {
		if _, err := b.oc.ensureSession(t.Context(), item.id, item.name, true); err != nil {
			t.Fatal(err)
		}
		expectValue(t, titles, "Lark: "+item.id)
	}
	const termID, id = "vocat-0002", "cake"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, termID, "Cake")
	mint := testConnectWS(t, base, "unit", false)
	mint.hello(t, "vocat-0003", "Mint")
	toolTest(t, base, "emit", object{"to": id, "chime": "tap"}, false)
	c.until(t, func(f wsFrame) bool {
		p := framePayload(f)
		return hasProto(msgChime, "")(f) && len(p) >= 1 && p[0] == chimeTap
	})
	select {
	case created := <-titles:
		t.Fatal("local-only tap started an AI session:", created)
	default:
	}
	b.ws.mu.Lock()
	b.ws.startTurn(b.ws.terminals[termID], time.Now(), false, "voice", func(ctx context.Context, turn uint64) {
		b.ws.runTurn(ctx, b.ws.terminals[termID], turn, "Next quiz round!", "voice", nil)
	})
	b.ws.mu.Unlock()
	expectValue(t, titles, "Lark: Cake (cake)")
	select {
	case text := <-prompts:
		if !strings.Contains(text, "\nNext quiz round!\n[state: ") {
			t.Fatal("turn did not submit its explicit prompt")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}
	if svg := waitSVG(t, b.ws, termID, "Done."); !strings.Contains(svg, "Answer") {
		t.Fatal("no-audio answer notice was missing")
	}
	if got, err := b.history.list(t.Context(), "mint"); err != nil || len(got) != 0 {
		t.Fatal("fenced prose triggered intercom", err, got)
	}
}
