package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParentPage(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.virtual = virtualStore(b.db)
		b.parentToken = testParentToken
	})
	page := testRequest(t, "GET", base+"/app/parent", "", nil, 200)
	if !bytes.Contains(page, []byte("Parent access")) {
		t.Fatal("parent page is missing its access form")
	}
	testRequest(t, "GET", base+"/app/parent.js", "", nil, 200)
	testRequest(t, "GET", base+"/app/parent-controls.html", "", nil, 404)
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Clover"}, 201))
	id := str(v["id"])
	fragment := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Lark-Token", "unit")
		r.Header.Set("X-Lark-Parent-Token", token)
		r.Header.Set("HX-Request", "true")
		r.Header.Set("Accept", "text/html")
		if body != "" {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		return w
	}
	if got := fragment("GET", "/api/parent", "", ""); got.Code != 403 {
		t.Fatalf("missing parent token: got %d", got.Code)
	}
	w := fragment("GET", "/api/parent?device_id="+id, testParentToken, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("parent fragment: %d %s", w.Code, w.Body)
	}
	for _, field := range []string{"Quiet hours and daily limit", "Activity in the last 7 days", "Open this companion's chat history", "Clover"} {
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("parent fragment is missing %q", field)
		}
	}
	path := "/api/devices/" + id + "/parent"
	if got := fragment("POST", path, testParentToken, "action=pause&minutes=1&minutes=2"); got.Code != 400 {
		t.Fatalf("duplicate mutation field: got %d", got.Code)
	}
	w = fragment("POST", path, testParentToken, "action=pause&minutes=15")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-variant="warning">Paused</span>`) || strings.Contains(w.Body.String(), testParentToken) {
		t.Fatalf("parent pause fragment: %d %s", w.Code, w.Body)
	}
}
