package main

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed web/*.html web/style.css web/*.js web/lark.svg
var webFiles embed.FS

const (
	oatCDN  = "https://unpkg.com/@knadh/oat@1.1.0/oat.min."
	htmxCDN = "https://unpkg.com/htmx.org@4.0.2/dist/htmx.min.js"
)

func serveApp(w http.ResponseWriter, path string) bool {
	var name, mime string
	switch path {
	case "/app":
		name, mime = "index.html", "text/html; charset=utf-8"
	case "/app/style.css":
		name, mime = "style.css", "text/css; charset=utf-8"
	case "/app/lark.svg", "/favicon.svg":
		name, mime = "lark.svg", "image/svg+xml"
	default:
		var ok bool
		name, ok = strings.CutPrefix(path, "/app/")
		if !ok || !strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".test.js") ||
			strings.ContainsAny(name, "/\\%") || strings.Contains(name, "..") {
			return false
		}
		mime = "text/javascript; charset=utf-8"
	}
	return serveAppFile(w, name, mime)
}

func serveAppFile(w http.ResponseWriter, name, mime string) bool {
	data, err := webFiles.ReadFile("web/" + name)
	if err != nil {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	scripts := "'self' " + oatCDN + "js"
	if name == "parent.html" {
		scripts += " " + htmxCDN
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src "+scripts+"; style-src 'self' "+oatCDN+"css; img-src 'self' data: blob:; manifest-src 'self'; worker-src 'self'; connect-src 'self' ws: wss:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "microphone=(self)")
	sendBytes(w, 200, mime, data)
	return true
}
