package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLarkCrestAsset(t *testing.T) {
	for _, path := range []string{"/app/lark.svg", "/favicon.svg"} {
		w := httptest.NewRecorder()
		if !serveApp(w, path) || w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" ||
			!strings.Contains(w.Body.String(), `id="crest"`) ||
			!strings.Contains(w.Body.String(), `viewBox="0 0 64 64"`) {
			t.Fatalf("%s does not serve the Storybook Lark crest", path)
		}
	}
}
