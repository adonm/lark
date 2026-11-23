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
	t.Parallel()
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

// TestREADMELoopbackClaims holds the README's "loopback + token only" promise
// for /mcp and /debug/pprof to the handler. A remote peer must see the same
// generic 404 as an unknown route, never a token challenge or profile.
func TestREADMELoopbackClaims(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "loopback") {
			continue
		}
		for _, path := range []string{"/mcp", "/debug/pprof"} {
			if strings.Contains(line, path) {
				documented[path] = true
			}
		}
	}
	for _, path := range []string{"/mcp", "/debug/pprof"} {
		if !documented[path] {
			t.Errorf("README's API table does not call %s loopback-only", path)
		}
	}

	b, _ := wsHarness(t, nil)
	requests := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{http.MethodGet, "/debug/pprof/", ""},
	}
	for _, rt := range requests {
		remote := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		remote.RemoteAddr = "198.51.100.7:23456"
		remote.Header.Set("X-Lark-Token", b.token)
		if rt.body != "" {
			remote.Header.Set("Content-Type", "application/json")
		}
		res := httptest.NewRecorder()
		b.ServeHTTP(res, remote)
		if res.Code != http.StatusNotFound || !strings.Contains(res.Body.String(), "Not found") {
			t.Errorf("%s %s from a non-loopback peer = %d %q; want the generic 404",
				rt.method, rt.path, res.Code, res.Body.String())
		}

		local := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
		local.RemoteAddr = "127.0.0.1:23456"
		local.Header.Set("X-Lark-Token", b.token)
		if rt.body != "" {
			local.Header.Set("Content-Type", "application/json")
		}
		res = httptest.NewRecorder()
		b.ServeHTTP(res, local)
		if res.Code == http.StatusNotFound {
			t.Errorf("%s %s from loopback = 404; documented as reachable", rt.method, rt.path)
		}
	}
}

// TestREADMEListsEveryRoute guards the other direction: a route the server
// registers must appear in README's API table.
func TestREADMEListsEveryRoute(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := []string{}
	routeRE := regexp.MustCompile("`(?:GET|POST|PATCH|PUT|DELETE) ([^`]+)`")
	inTable := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "| Lark · port 8765 |") {
			inTable = true
			continue
		}
		if inTable && !strings.HasPrefix(line, "|") {
			break
		}
		if !inTable {
			continue
		}
		for _, m := range routeRE.FindAllStringSubmatch(line, -1) {
			p := strings.SplitN(m[1], "?", 2)[0]
			if i := strings.IndexByte(p, '{'); i >= 0 {
				p = strings.TrimSuffix(p[:i], "/")
			}
			documented = append(documented, strings.TrimSuffix(p, "/"))
		}
	}
	if len(documented) == 0 {
		t.Fatal("parsed zero documented routes")
	}
	covered := func(route string) bool {
		route = strings.TrimSuffix(strings.SplitN(route, "?", 2)[0], "/")
		for _, doc := range documented {
			if route == doc || strings.HasPrefix(route, doc+"/") || strings.HasPrefix(doc, route+"/") {
				return true
			}
		}
		return false
	}
	routeCode := regexp.MustCompile(`(?:path ==|HasPrefix\(path,)\s*"(/[^"]*)"`)
	checked := 0
	for _, name := range []string{"handlers.go", "companion.go", "terminal.go", "remote.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range routeCode.FindAllStringSubmatch(string(src), -1) {
			route := m[1]
			if strings.HasPrefix(route, "/api/portraits/") || strings.HasPrefix(route, "/remote/api/") {
				continue // Documented as one scoped-command row.
			}
			checked++
			if !covered(route) {
				t.Errorf("route %q is registered but missing from README's API table", route)
			}
		}
	}
	if checked == 0 {
		t.Fatal("parsed zero registered routes")
	}
}
