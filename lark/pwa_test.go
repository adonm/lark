package main

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPWAInstallAssets(t *testing.T) {
	b, base := wsHarness(t, nil)
	v := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{"name": "Violet"}, 201))
	id := str(v["id"])
	companion, ok := b.companionByID(id)
	if !ok {
		t.Fatal("companion not created")
	}
	login := httptest.NewRequest("GET", "/api/scope", nil)
	login.Header.Set("X-Lark-Token", b.token)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, login)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != pwaPath || !cookies[0].Secure || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Value == b.token {
		t.Fatal("install artwork session is not protected")
	}
	cookie := cookies[0]
	const manifest = pwaPath + "manifest.webmanifest"
	const grant = "scoped-pwa-fixture"
	scoped := b.scoped(companion, grant)
	request := func(t *testing.T, handler http.Handler, path string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response: got status %d, wanted %d, cache policy %q", w.Code, status, w.Header().Get("Cache-Control"))
		}
		return w
	}
	request(t, b, manifest+"?companion_id="+id, nil, 401)
	request(t, b, "/api/companions", cookie, 401)
	request(t, b, manifest+"?companion_id=missing", cookie, 404)
	request(t, scoped, manifest, cookie, 401)
	request(t, scoped, manifest+"?token="+grant+"&companion_id=missing", nil, 403)
	request(t, scoped, pwaPath+"icon-192.png?companion_id="+id, cookie, 401)
	request(t, scoped, "/app/parent", nil, 404)
	assertAsset := request(t, scoped, "/app/lark.svg", nil, 200)
	if assertAsset.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatal("wrong favicon MIME type")
	}
	for _, tc := range []struct {
		name    string
		handler http.Handler
		query   string
		cookie  *http.Cookie
	}{
		{"LAN", b, "?companion_id=" + id, cookie},
		{"scoped", scoped, "?token=" + grant, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := request(t, tc.handler, manifest+tc.query, tc.cookie, 200)
			if m.Header().Get("Content-Type") != "application/manifest+json" || m.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("manifest headers missing")
			}
			obj := testObject(t, m.Body.Bytes())
			u, err := url.Parse(str(obj["start_url"]))
			if err != nil || u.IsAbs() || u.Path != "/app" || u.RawQuery != tc.query[1:] || obj["scope"] != "/app" ||
				obj["name"] != "Violet" || obj["id"] != "/app?companion_id="+id || obj["display"] != "standalone" ||
				obj["orientation"] != "portrait" || obj["theme_color"] != storybookPalettes["peach"].furShadow || obj["background_color"] != storybookPalettes["peach"].bg {
				t.Fatal("manifest metadata or launch scope is incorrect")
			}
			icons, _ := obj["icons"].([]any)
			if len(icons) != 3 {
				t.Fatal("manifest requires two standard icons and a maskable icon")
			}
			for i, size := range []int{192, 512, 512} {
				icon := icons[i].(map[string]any)
				src := str(icon["src"])
				if !strings.HasPrefix(src, pwaPath) || !strings.HasSuffix(src, tc.query) ||
					(icon["purpose"] == "maskable") != (i == 2) {
					t.Fatal("incorrect icon path or maskable purpose")
				}
				r := request(t, tc.handler, src, tc.cookie, 200)
				img, err := png.Decode(r.Body)
				if err != nil || img.Bounds().Dx() != size || img.Bounds().Dy() != size || r.Header().Get("Content-Type") != "image/png" {
					t.Fatalf("icon %d is not a %d px PNG", i, size)
				}
				corner := img.At(0, 0)
				red, green, blue, alpha := corner.RGBA()
				if alpha != 65535 || red>>8 != 0xff || green>>8 != 0xf2 || blue>>8 != 0xe7 {
					t.Fatal("icon must have an opaque palette background")
				}
				hasArt := false
				for x := size / 4; x < size*3/4 && !hasArt; x++ {
					for y := size / 4; y < size*3/4 && !hasArt; y++ {
						hasArt = img.At(x, y) != corner
					}
				}
				if !hasArt {
					t.Fatal("cat art missing from icon")
				}
			}
		})
	}
	blank := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{}, 201))
	blankID := str(blank["id"])
	quick := request(t, b, manifest+"?companion_id="+blankID, cookie, 200)
	quickApp := testObject(t, quick.Body.Bytes())
	if quickApp["name"] != "Lark" || quickApp["start_url"] != "/app?companion_id="+blankID {
		t.Fatal("blank companion PWA metadata mismatch", quickApp)
	}
	lanWorker := request(t, b, "/app/sw.js", nil, 200)
	privateWorker := request(t, scoped, "/app/sw.js", nil, 200)
	if lanWorker.Header().Get("Service-Worker-Allowed") != "/app" || privateWorker.Header().Get("Service-Worker-Allowed") != "/app" ||
		strings.Contains(privateWorker.Body.String(), grant) || bytes.Equal(lanWorker.Body.Bytes(), privateWorker.Body.Bytes()) ||
		!strings.Contains(privateWorker.Body.String(), `"/app/lark.svg"`) ||
		!strings.Contains(privateWorker.Body.String(), `"lark-shell-scoped-`) {
		t.Fatal("worker scope or shell allowlist is incorrect")
	}
}

func TestWebTerminalReconnectDoesNotReplaceLiveTab(t *testing.T) {
	b, base := wsHarness(t, nil)
	v := testObject(t, testRequest(t, "POST", base+"/api/companions", "unit", object{}, 201))
	id := str(v["id"])
	companion, _ := b.companionByID(id)
	term := testObject(t, testRequest(t, "POST", base+"/api/terminals", "unit", object{"kind": "web", "companion_id": id}, 201))
	termID := str(term["id"])
	const grant = "scoped-hello-fixture"
	scoped := httptest.NewServer(b.scoped(companion, grant))
	defer scoped.Close()
	first := testConnectWS(t, base, "unit", false)
	first.helloWeb(t, termID, id, capDrag, false)
	second := testConnectWS(t, scoped.URL, grant, true)
	second.helloWeb(t, termID, "", capDrag, false) // An explicit new join can still take over.
	closeFrame := func(socket *testWS) {
		t.Helper()
		f := socket.until(t, func(f wsFrame) bool { return f.op == wsClose })
		if !bytes.Equal(f.data, wsCloseFrame(wsCompanionOccupied).data) {
			t.Fatal("replaced or rejected socket did not receive occupied close code")
		}
	}
	closeFrame(first)
	join := func() *testWS {
		s := testConnectWS(t, base, "unit", false)
		s.sendProto(t, msgHello, encodeTerminalHello(terminalHello{
			protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG,
			flags: helloFlagIfFree, terminalID: termID, companionID: id,
		}))
		return s
	}
	closeFrame(join())
	second.barrier(t)
	watcher := testConnectWS(t, base, "unit", false)
	watcher.sendProto(t, msgHello, encodeTerminalHello(terminalHello{protoVersion: protoVersion1, kind: termKindWeb, displayFormats: dispSVG}))
	watcher.until(t, hasProto(msgPeers, id))
	second.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
	second.until(t, func(f wsFrame) bool { return f.op == wsClose })
	// detach happens after the close handshake. A watcher sees the terminal go offline.
	watcher.until(t, hasProto(msgPeers, ""))
	join().until(t, hasSVG(id))
}
