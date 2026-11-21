package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"time"
)

var pairingCodeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

type pairingLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type pairingResponse struct {
	OK        bool          `json:"ok"`
	Links     []pairingLink `json:"links"`
	ExpiresAt time.Time     `json:"expires_at"`
}

func (b *larkServer) pairOpenCode(r *http.Request) (pairingResponse, error) {
	if b.pairPort == 0 || b.oc == nil {
		return pairingResponse{}, problem(503, "Dedicated OpenCode pairing is not configured")
	}
	// Use the listener address the client reached, never a caller-supplied Host.
	local, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if local == nil {
		return pairingResponse{}, problem(400, "Open the dashboard directly on your LAN")
	}
	ip, _ := netip.AddrFromSlice(local.IP)
	ip = ip.Unmap()
	if !ip.Is4() || !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return pairingResponse{}, problem(400, "Open the dashboard at its local IPv4 address")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result struct {
		Code      string `json:"code"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := b.oc.jsonCall(ctx, http.MethodPost, "/api/pair", nil, &result); err != nil {
		return pairingResponse{}, err
	}
	if !pairingCodeRE.MatchString(result.Code) || result.ExpiresIn < 1 || result.ExpiresIn > 86400 {
		return pairingResponse{}, b.oc.invalid()
	}
	return pairingResponse{true, []pairingLink{{"OpenCode",
		"http://" + net.JoinHostPort(ip.String(), strconv.Itoa(b.pairPort)) + "/auth/connect/" + result.Code}},
		time.Now().UTC().Add(time.Duration(result.ExpiresIn) * time.Second)}, nil
}
