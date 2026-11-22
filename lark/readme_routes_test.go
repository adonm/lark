package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestREADMERoutesRegistered(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	inTable := false
	routeRE := regexp.MustCompile("`(GET|POST|PATCH|PUT|DELETE) ([^`]+)`")
	type route struct{ method, path string }
	var routes []route
	for _, line := range lines {
		if strings.HasPrefix(line, "| Lark · port 8765 |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		for _, m := range routeRE.FindAllStringSubmatch(line, -1) {
			routes = append(routes, route{m[1], m[2]})
		}
	}
	if len(routes) == 0 {
		t.Fatal("parsed zero routes from README.md")
	}

	b, _ := wsHarness(t, nil)
	for _, rt := range routes {
		p := rt.path
		p = strings.ReplaceAll(p, "{id}", "cat")
		if strings.HasPrefix(p, "/api/terminals/") {
			p = strings.ReplaceAll("/api/terminals/vocat-0001"+strings.TrimPrefix(p, "/api/terminals/cat"), "cat", "vocat-0001")
		}
		var body *strings.Reader
		if rt.method == http.MethodPost || rt.method == http.MethodPatch || rt.method == http.MethodPut {
			body = strings.NewReader("{}")
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(rt.method, p, body)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Lark-Token", b.token)
		if body.Len() > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		res := httptest.NewRecorder()
		b.ServeHTTP(res, req)
		if res.Code == http.StatusNotFound && strings.Contains(res.Body.String(), `"Not found"`) {
			t.Errorf("documented route %s %s returned generic 404 Not found", rt.method, rt.path)
		}
	}
}
