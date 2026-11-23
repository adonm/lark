package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChatHistoryCloseDuringConcurrentAdd(t *testing.T) {
	b, _ := wsHarness(t, nil)
	for range 200 {
		h := &chatHistory{db: b.db}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, id := range []string{"greg", "cake"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range 8 {
					h.add(id, newChat("user", "You", "hello", "typed"))
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			h.close()
		}()
		close(start)
		wg.Wait()
	}
}

func TestDeviceResetHistory(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, dir string) {
		b.oc = testOpenCodeOnDB(t, b.root, dir, b.db, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_reset/interrupt" {
				fmt.Fprint(w, `{"data":{"interrupted":false}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	v := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Milo", "form": "a cat"}, 201))
	id := str(v["id"])
	path := base + "/api/companions/" + id
	const other = "other-companion"
	for _, e := range []chatEntry{newChat("user", "You", "Hi", "typed"), newChat("companion", "Milo", "Hello", "typed")} {
		b.history.add(id, e)
		b.history.add(other, newChat(e.Role, e.Sender, "Other chat", e.Source))
	}
	read := func(key string) []chatEntry {
		t.Helper()
		got, err := b.history.list(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	assertHistory := func(key string, want []chatEntry) {
		t.Helper()
		if got := read(key); !slices.Equal(got, want) {
			t.Fatalf("history for %s: got %v, want %v", key, got, want)
		}
	}
	original, untouched := read(id), read(other)
	if len(original) != 2 || len(untouched) != 2 {
		t.Fatal("fixture history not stored")
	}
	for _, tc := range []struct {
		body any
		code int
		sid  string
	}{
		{object{"archive_history": "true"}, 400, "ses_reset"},
		{nil, 200, ""},
		{object{}, 200, ""},
		{object{"archive_history": false}, 200, ""},
		{object{"archive_history": true}, 200, ""},
	} {
		if err := b.oc.save(id, "ses_reset"); err != nil {
			t.Fatal(err)
		}
		testRequest(t, "POST", path+"/reset", "unit", tc.body, tc.code)
		if sid, err := b.oc.get(id); err != nil || sid != tc.sid {
			t.Fatalf("reset %v: session = %q, %v; want %q", tc.body, sid, err, tc.sid)
		}
		if body, ok := tc.body.(object); !ok || body["archive_history"] != true {
			assertHistory(id, original)
		}
		assertHistory(other, untouched)
	}
	if got := read(id); len(got) != 0 {
		t.Fatal("reset device still exposes archived history")
	}
	var archive string
	if err := b.db.QueryRow(`SELECT companion_id FROM messages WHERE id=?`, original[0].ID).Scan(&archive); err != nil {
		t.Fatal(err)
	}
	stamp, ok := strings.CutPrefix(archive, "archive-"+id+"-")
	if _, err := strconv.ParseInt(stamp, 10, 64); !ok || err != nil {
		t.Fatalf("archive key = %q; want timestamped key for %s", archive, id)
	}
	assertHistory(archive, original)
	b.history.add(id, newChat("user", "You", "Fresh chat", "typed"))
	if got := read(id); len(got) != 1 {
		t.Fatalf("new chat has %d messages, want 1", len(got))
	}
	assertHistory(archive, original)
}

func TestHistoryCursorPagination(t *testing.T) {
	b, _ := wsHarness(t, nil)
	const id = "lark-history"
	tx, err := b.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	start := time.UnixMilli(1700000000000)
	for i := 504; i >= 0; i-- {
		// Deliberately insert ties in reverse ID order.
		e := chatEntry{ID: fmt.Sprintf("msg-%03d", i), Role: "user", Text: "Hello", Timestamp: start.Add(time.Duration(i/3) * time.Millisecond)}
		if _, err := tx.Exec(insertMessage, messageValues(id, e)...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(insertMessage, messageValues("another-board", newChat("user", "You", "Different board", "typed"))...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	read := func(limit int, before *historyCursor) (p historyPage) {
		t.Helper()
		p, err := b.readHistoryPage(t.Context(), id, limit, before)
		if err != nil {
			t.Fatal(err)
		}
		if p.CompanionID != id || p.HasMore != (p.Next != nil) {
			t.Fatal(p)
		}
		return
	}
	if p := read(100, nil); len(p.Messages) != 100 || p.Next == nil || p.Next.ID != "msg-405" || p.Messages[99].ID != "msg-504" {
		t.Fatalf("unexpected default page: %d messages, cursor %v", len(p.Messages), p.Next)
	}
	p := read(500, nil)
	if len(p.Messages) != 500 || p.Next == nil || p.Next.ID != "msg-005" {
		t.Fatalf("unexpected max page: %d messages, cursor %v", len(p.Messages), p.Next)
	}
	p = read(3, p.Next)
	if len(p.Messages) != 3 || p.Messages[0].ID != "msg-002" || p.Messages[2].ID != "msg-004" || p.Next == nil {
		t.Fatalf("same timestamp IDs were skipped: %+v", p)
	}
	p = read(3, p.Next)
	if len(p.Messages) != 2 || p.Messages[0].ID != "msg-000" || p.Messages[1].ID != "msg-001" || p.HasMore {
		t.Fatalf("unexpected last page: %+v", p)
	}
}

func TestSessionMatchesDevice(t *testing.T) {
	for _, tc := range []struct {
		title, device string
		want          bool
	}{
		{"Lark: lark-33d8", "lark-33d8", true},
		{"Lark: scope-virt-1234", "virt-1234", true},
		{"Lark: Cake (lark-33d8)", "LARK-33D8", true},
		{"Lark: lark-33d80", "lark-33d8", false},
		{"Lark: scope-lark-33d80", "lark-33d8", false},
		{"Lark: Cake (lark-33d80)", "lark-33d8", false},
		{"Lark: Cake (lark-33d8) archived", "lark-33d8", false},
		{"Lark: Cake(lark-33d8)", "lark-33d8", false},
		{"Other: Cake (lark-33d8)", "lark-33d8", false},
	} {
		if got := sessionMatchesDevice(tc.title, tc.device); got != tc.want {
			t.Errorf("sessionMatchesDevice(%q, %q) = %t, want %t", tc.title, tc.device, got, tc.want)
		}
	}
}

func TestListDeviceSessionsFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		activeSID string
		want      []string
		wantError bool
	}{
		{"unlisted active", http.StatusOK, "ses_active", []string{"ses_old", "ses_active"}, false},
		{"no active", http.StatusOK, "", []string{"ses_old"}, false},
		{"unavailable with active", http.StatusServiceUnavailable, "ses_active", []string{"ses_active"}, false},
		{"unavailable without active", http.StatusServiceUnavailable, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := historyOpenCode(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/session" {
					t.Error("unexpected path", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"data":[{"id":"ses_old","title":"Lark: lark-history","time":{"created":100}}]}`)
			})
			got, err := o.ListDeviceSessions(t.Context(), "lark-history", tc.activeSID)
			if !slices.Equal(got, tc.want) || (err != nil) != tc.wantError {
				t.Fatalf("ListDeviceSessions = %v, %v; want %v (error: %t)", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestSessionMessageBackfillExceedsOldPaginationLimit(t *testing.T) {
	o := historyOpenCode(t, func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			var err error
			offset, err = strconv.Atoi(cursor)
			if err != nil || offset > 500 {
				t.Error("unexpected cursor", cursor)
				http.Error(w, "unexpected cursor", 400)
				return
			}
		}
		data := make([]object, 50)
		for i := range data {
			data[i] = object{"id": fmt.Sprintf("msg_%d", offset+i), "type": "user",
				"text": "Example", "time": object{"created": offset + i}}
		}
		next := ""
		if offset < 500 {
			next = strconv.Itoa(offset + len(data))
		}
		_ = json.NewEncoder(w).Encode(object{"data": data, "cursor": object{"next": next}})
	})
	messages, err := o.GetSessionMessages(t.Context(), "ses_test")
	if err != nil || len(messages) != 550 || messages[0].ID != "msg_0" || messages[549].ID != "msg_549" {
		t.Fatalf("expected all 550 session messages, got %d (%v)", len(messages), err)
	}
}

func historyOpenCode(t *testing.T, h http.HandlerFunc) *openCode {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "service.json")
	if err := writeMap(path, object{"url": server.URL, "password": "unit"}); err != nil {
		t.Fatal(err)
	}
	return &openCode{upstream: upstream{"OpenCode", server.Client(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}}}
}
