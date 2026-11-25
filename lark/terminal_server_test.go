package main

import (
	"bufio"
	"context"
	"net"
	"testing"
	"time"
)

// A sleeping panel must not have its radio woken every 12 s; waking returns to
// the full liveness rate.
func TestPingIntervalFollowsSleep(t *testing.T) {
	t.Parallel()
	b, _ := wsHarness(t, nil)
	srv, err := newTerminalServer(b.hub, "127.0.0.1", "0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown()

	const id = "lark-ping1"
	dev, reject := dialNativeDevice(t, srv, id, "unit", 0)
	if dev == nil {
		t.Fatalf("terminal rejected: %q", reject)
	}

	d := b.hub.live(id)
	if d == nil || d.client.native == nil {
		t.Fatal("terminal did not register a native client")
	}
	nc := d.client.native

	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("awake ping interval = %v, want %v", got, pingAwake)
	}
	updateTerm(t, d, func(d *terminalSession) { b.hub.setSleeping(d, true) })
	if got := nc.pingInterval(); got != pingAsleep {
		t.Fatalf("sleeping ping interval = %v, want %v", got, pingAsleep)
	}
	updateTerm(t, d, func(d *terminalSession) { b.hub.setSleeping(d, false) })
	if got := nc.pingInterval(); got != pingAwake {
		t.Fatalf("woken ping interval = %v, want %v", got, pingAwake)
	}
}

func TestControlQueueOverflowDropsInsteadOfDisconnecting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nc := &nativeClient{
		ctrlOut:     make(chan nativeOutMsg, 1),
		out:         make(chan nativeOutMsg, 1),
		frameNotify: make(chan struct{}, 1),
		client:      &terminalClient{ctx: ctx, cancel: cancel},
	}
	if !nc.enqueue(msgPing, []byte{1}) {
		t.Fatal("first control frame was not queued")
	}
	if nc.enqueue(msgPing, []byte{2}) {
		t.Fatal("overflowing control frame was queued")
	}
	if ctx.Err() != nil {
		t.Fatal("control overflow disconnected the client")
	}
	if got := nc.droppedCtrl.Load(); got != 1 {
		t.Fatalf("dropped controls = %d, want 3", got)
	}

	// PCM keeps the old behaviour: the client cannot drain audio, so the
	// stream is unusable.
	if !nc.enqueue(msgTTSPCM, []byte{3}) {
		t.Fatal("first PCM frame was not queued")
	}
	if nc.enqueue(msgTTSPCM, []byte{4}) {
		t.Fatal("overflowing PCM frame was queued")
	}
	if ctx.Err() == nil {
		t.Fatal("PCM overflow did not end the connection")
	}
}

func TestFramesFlushBetweenPCMChunks(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	nc := &nativeClient{
		conn:        server,
		client:      &terminalClient{ctx: ctx, cancel: cancel},
		ctrlOut:     make(chan nativeOutMsg, 4),
		out:         make(chan nativeOutMsg, 8),
		frameNotify: make(chan struct{}, 1),
	}
	nc.enqueue(msgFrame, []byte("frame"))
	for i := range 3 {
		nc.enqueue(msgTTSPCM, []byte{byte('a' + i)})
	}
	go nc.writeLoop()

	_ = client.SetReadDeadline(time.Now().Add(testWait(t)))
	br := bufio.NewReader(client)
	var order []uint8
	for range 4 {
		msgType, _, _, err := readProtoFrame(br)
		if err != nil {
			t.Fatalf("read frame %d: %v", len(order), err)
		}
		order = append(order, msgType)
	}
	want := []uint8{msgTTSPCM, msgFrame, msgTTSPCM, msgTTSPCM}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("frame order = %v, want %v", order, want)
		}
	}
}

func TestRTPDropsDuplicatesAndReordering(t *testing.T) {
	t.Parallel()
	nc := &nativeClient{}
	for _, tc := range []struct {
		seq    uint16
		accept bool
	}{
		{100, true},
		{100, false}, // duplicate
		{99, false},  // older
		{101, true},
		{103, true}, // a gap is a loss, not a reorder
		{102, false},
	} {
		if got := nc.acceptRTP(tc.seq); got != tc.accept {
			t.Fatalf("acceptRTP(%d) = %v, want %v", tc.seq, got, tc.accept)
		}
	}
	if got := nc.rtpDrops.Load(); got != 3 {
		t.Fatalf("dropped RTP packets = %d, want 3", got)
	}
	wrap := &nativeClient{}
	for _, tc := range []struct {
		seq    uint16
		accept bool
	}{{65535, true}, {0, true}, {65535, false}, {0, false}} {
		if got := wrap.acceptRTP(tc.seq); got != tc.accept {
			t.Fatalf("wrapped acceptRTP(%d) = %v, want %v", tc.seq, got, tc.accept)
		}
	}
}

func TestLosslessTTSAcksByFirmware(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		firmware string
		want     bool
	}{
		{"0.21.0", true},
		{"0.21.0-test", true},
		{"0.22.1", true},
		{"0.20.9", false},
		{"sim-0.19.0", false},
		{"", false},
		{"0.21", true},
	} {
		if got := losslessTTSAcks(tc.firmware); got != tc.want {
			t.Fatalf("losslessTTSAcks(%q) = %v, want %v", tc.firmware, got, tc.want)
		}
	}
}
