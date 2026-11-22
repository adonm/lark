package main

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppCDNPolicy(t *testing.T) {
	b := &larkServer{}
	scoped := b.scoped(companionState{ID: "cake"}, "scoped-fixture")
	for _, view := range []struct {
		path   string
		scoped bool
	}{
		{"/app", false},
		{"/app", true},
	} {
		r := httptest.NewRequest("GET", view.path, nil)
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		if view.scoped {
			scoped.ServeHTTP(w, r)
		} else {
			b.ServeHTTP(w, r)
		}
		policy := w.Header().Get("Content-Security-Policy")
		if w.Code != 200 || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("app and private page referrers must not be shared")
		}
		for _, asset := range []string{oatCDN + "css", oatCDN + "js"} {
			if !strings.Contains(policy, asset) ||
				!bytes.Contains(w.Body.Bytes(), []byte(`="`+asset+`" integrity="sha384-`)) {
				t.Errorf("%+v: CDN asset %s missing integrity or allowed on the wrong surface", view, asset)
			}
		}
		if strings.Contains(policy, "https://unpkg.com ") || strings.Contains(policy, "'unsafe-") {
			t.Errorf("unexpected CSP sources: %s", policy)
		}
	}
}
