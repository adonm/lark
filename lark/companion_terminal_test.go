package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompanionTerminalLifecycle(t *testing.T) {
	b, base := wsHarness(t, nil)

	// 1. Create two companions; both start dormant with zero terminals.
	cake := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Cake"}, 201))
	mint := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Mint"}, 201))
	cakeID, mintID := str(cake["id"]), str(mint["id"])
	if cake["dormant"] != true || mint["dormant"] != true {
		t.Fatalf("new companions should be dormant: cake=%v mint=%v", cake, mint)
	}

	// 2. Connect hardware terminal vocat-33d8 (attaches to Cake by slug).
	hw := testConnectWS(t, base, "unit", false)
	hw.helloV2(t, "vocat-33d8", "Cake")

	// 3. Browser web terminal created by hello attached to Cake -> Cake now has two terminals.
	const webID = "web-11223344"
	web := testConnectWS(t, base, "unit", true)
	web.helloWeb(t, webID, cakeID, capDrag, false)

	if _, ok := b.terminalByID(webID); !ok {
		t.Fatal("browser hello did not persist web terminal")
	}

	// 4. Companion with two terminals: both receive frames when companion changes or app runs.
	toolTest(t, base, "patch", object{"companion_id": cakeID, "companion": object{"face_bg": "#112233"}}, false)
	hw.until(t, isDisplay)
	web.until(t, hasSVG("#112233"))

	toolTest(t, base, "app", object{"companions": cakeID, "svg": `<svg><text x="180" y="180">SharedScreen</text></svg>`}, false)
	hw.until(t, isDisplay)
	web.until(t, hasSVG("SharedScreen"))

	// Cannot delete a companion that still has attached terminals.
	testRequest(t, http.MethodDelete, base+"/api/companions/"+cakeID, "unit", nil, 409)

	// 5. Move hardware terminal vocat-33d8 from Cake to Mint.
	moved := testObject(t, testRequest(t, http.MethodPost, base+"/api/terminals/vocat-33d8/attach", "unit", object{"companion_id": mintID}, 200))
	if moved["companion_id"] != mintID {
		t.Fatalf("terminal did not move to mint: %v", moved)
	}
	hw.until(t, isDisplay)

	// 6. Detach web terminal from Cake -> Cake becomes dormant and can be deleted.
	detached := testObject(t, testRequest(t, http.MethodPost, base+"/api/terminals/"+webID+"/detach", "unit", object{}, 200))
	if detached["companion_id"] != "" {
		t.Fatalf("terminal did not detach: %v", detached)
	}
	gotCake := testObject(t, testRequest(t, http.MethodGet, base+"/api/companions/"+cakeID, "unit", nil, 200))
	if gotCake["dormant"] != true {
		t.Fatalf("cake should be dormant after detaching all terminals: %v", gotCake)
	}
	testRequest(t, http.MethodDelete, base+"/api/companions/"+cakeID, "unit", nil, 204)
}

func TestCompanionIntercomCallAndTunnelScope(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.worker = fakeTTS(b.root, nil)
	})
	cake := callClient(t, b, base, "cake", "Cake")
	mint := callClient(t, b, base, "mint", "Mint")

	// Intercom addressed by companion.
	intercom := toolTest(t, base, "emit", object{"to": "mint", "intercom": object{"from": "cake", "text": "Tea time!"}}, false)
	if r := intercom["receipts"].([]any)[0].(object); r["companion_id"] != "mint" || r["status"] != "queued" {
		t.Fatalf("unexpected intercom receipt: %v", intercom)
	}
	mint.until(t, hasSVG("Tea time!"))

	// Call addressed by companion.
	callOut := toolTest(t, base, "emit", object{"to": "mint", "call": object{"from": "cake"}}, false)
	callID := callOut["call"].(object)["id"]
	mint.until(t, hasSVG("call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, hasProto(msgCallCtrl, ""))
	toolTest(t, base, "emit", object{"to": "mint", "call": object{"op": "hangup", "id": callID}}, false)
	cake.until(t, hasProto(msgCallCtrl, ""))

	// Tunnel scope restricted to companion.
	c, _ := b.companionByID("mint")
	scoped := httptest.NewServer(b.scoped(c, "mint-token"))
	defer scoped.Close()
	testRequest(t, http.MethodGet, scoped.URL+"/api/companions/mint/messages", "mint-token", nil, 200)
	testRequest(t, http.MethodGet, scoped.URL+"/api/companions/cake/messages", "mint-token", nil, 403)
	testRequest(t, http.MethodPost, scoped.URL+"/api/companions/cake/voice", "mint-token", object{"voice": fableVoice}, 403)
	testRequest(t, http.MethodGet, scoped.URL+"/api/companions", "mint-token", nil, 404)
	testRequest(t, http.MethodGet, scoped.URL+"/api/terminals", "mint-token", nil, 404)
}
