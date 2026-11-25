package main

import (
	"strings"
	"testing"
)

func TestDeltaChatInviteShortening(t *testing.T) {
	t.Parallel()
	const rawInvite = "https://i.delta.chat/#72120f6660ef1a2151b318904197b15e&v=1&n=Greg&i=abcdef1234567890&s=1234567890abcdef&a=greg%40example.org"
	invite := shortenDeltaChatInvite(rawInvite)
	if strings.Contains(invite, "&n=") || strings.Contains(invite, "&v=") {
		t.Fatalf("shortenDeltaChatInvite did not strip optional params: %q", invite)
	}
}
