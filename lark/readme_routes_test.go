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
