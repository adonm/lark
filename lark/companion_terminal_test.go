package main

import (
	"net/http"
	"testing"
)

func TestCompanionTerminalLifecycle(t *testing.T) {
	b, base := wsHarness(t, nil)

	// 1. Create two companions; both start dormant with zero terminals.
	cake := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Cake", "form": "a cat"}, 201))
	mint := testObject(t, testRequest(t, http.MethodPost, base+"/api/companions", "unit", object{"name": "Mint", "form": "a cat"}, 201))
	cakeID, mintID := str(cake["id"]), str(mint["id"])
	if cake["dormant"] != true || mint["dormant"] != true {
		t.Fatalf("new companions should be dormant: cake=%v mint=%v", cake, mint)
	}

	// 2. Connect hardware terminal vocat-33d8 (attaches to Cake by slug).
	hw := testConnectWS(t, base, "unit", false)
	hw.helloV2(t, "vocat-33d8", "Cake")

	// 3. Second hardware terminal attached to Cake -> Cake now has two terminals.
	const secondID = "vocat-2cac"
	second := testConnectWS(t, base, "unit", false)
	second.helloV2(t, secondID, cakeID)

	if _, ok := b.terminalByID(secondID); !ok {
		t.Fatal("second hello did not persist terminal")
	}

	// 4. Companion with two terminals: both receive frames when companion changes or app runs.
	toolTest(t, base, "patch", object{"companion_id": cakeID, "companion": object{"name": "Cake Two"}}, false)
	hw.until(t, isDisplay)

	toolTest(t, base, "app", object{"companions": cakeID, "code": `function render(s,p,g){g.text(20,20,"SharedScreen")}`}, false)
	hw.until(t, isDisplay)
	waitSVG(t, b.ws, secondID, "SharedScreen")

	// Cannot delete a companion that still has attached terminals.
	testRequest(t, http.MethodDelete, base+"/api/companions/"+cakeID, "unit", nil, 409)

	// 5. Move hardware terminal vocat-33d8 from Cake to Mint.
	moved := testObject(t, testRequest(t, http.MethodPost, base+"/api/terminals/vocat-33d8/attach", "unit", object{"companion_id": mintID}, 200))
	if moved["companion_id"] != mintID {
		t.Fatalf("terminal did not move to mint: %v", moved)
	}
	hw.until(t, isDisplay)

	// 6. Detach second terminal from Cake -> Cake becomes dormant and can be deleted.
	detached := testObject(t, testRequest(t, http.MethodPost, base+"/api/terminals/"+secondID+"/detach", "unit", object{}, 200))
	if detached["companion_id"] != "" {
		t.Fatalf("terminal did not detach: %v", detached)
	}
	gotCake := testObject(t, testRequest(t, http.MethodGet, base+"/api/companions/"+cakeID, "unit", nil, 200))
	if gotCake["dormant"] != true {
		t.Fatalf("cake should be dormant after detaching all terminals: %v", gotCake)
	}
	testRequest(t, http.MethodDelete, base+"/api/companions/"+cakeID, "unit", nil, 204)
}

func TestCompanionIntercom(t *testing.T) {
	b, base := wsHarness(t, func(b *larkServer, _ string) {
		b.worker = fakeTTS(b.root, nil)
	})
	_, _ = b.createCompanion(t.Context(), object{"id": "cake", "name": "Cake", "form": "a cat"})
	_, _ = b.createCompanion(t.Context(), object{"id": "mint", "name": "Mint", "form": "a cat"})
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "vocat-0001", "cake")
	mint := testConnectWS(t, base, "unit", false)
	mint.hello(t, "vocat-0002", "mint")

	// Intercom addressed by companion.
	intercom := toolTest(t, base, "emit", object{"to": "mint", "intercom": object{"from": "cake", "text": "Tea time!"}}, false)
	if r := intercom["receipts"].([]any)[0].(object); r["companion_id"] != "mint" || r["status"] != "queued" {
		t.Fatalf("unexpected intercom receipt: %v", intercom)
	}
	waitSVG(t, b.ws, "vocat-0002", "Tea time!")
	_ = cake
}
