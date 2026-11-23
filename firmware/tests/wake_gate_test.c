#include "wake_gate.h"

#include <assert.h>
#include <stdio.h>

// Loopback socket model: a FIFO of single-byte wake datagrams.
typedef struct {
    wake_gate_t *gate;
    int datagrams;
    int recv_calls;
    int race_at_call;    // queue another datagram from inside this recv call
    int check_pending_at; // recv call entry where the gate must still be set
    bool seen_pending;
} fake_socket_t;

static int fake_recv(void *ctx, void *buf, size_t len)
{
    fake_socket_t *sock = ctx;
    assert(len >= 1 && buf);
    ++sock->recv_calls;
    if (sock->check_pending_at == sock->recv_calls) {
        sock->seen_pending = atomic_load_explicit(&sock->gate->pending, memory_order_acquire);
    }
    if (sock->race_at_call == sock->recv_calls) {
        ++sock->datagrams;
    }
    if (!sock->datagrams) return -1; // EWOULDBLOCK
    --sock->datagrams;
    *(uint8_t *)buf = 1;
    return 1;
}

static void test_clears_only_after_drain(void)
{
    wake_gate_t gate = { 0 };
    fake_socket_t sock = { .gate = &gate, .datagrams = 2, .check_pending_at = 3 };
    assert(wake_gate_request(&gate));
    assert(!wake_gate_request(&gate)); // coalesced while one is in flight
    wake_gate_drain(&gate, fake_recv, &sock);
    // Two datagrams plus the final EWOULDBLOCK call, with the flag still set
    // until the socket is empty.
    assert(sock.recv_calls == 3);
    assert(sock.seen_pending);
    assert(!atomic_load(&gate.pending));
    assert(!sock.datagrams);
    // A request after the drain sends again.
    assert(wake_gate_request(&gate));
}

static void test_race_during_drain_keeps_wakeup(void)
{
    wake_gate_t gate = { 0 };
    fake_socket_t sock = { .gate = &gate, .datagrams = 1, .race_at_call = 1 };
    assert(wake_gate_request(&gate));
    wake_gate_drain(&gate, fake_recv, &sock);
    // The datagram queued by the racing producer is drained in the same pass,
    // so clearing afterwards cannot strand it.
    assert(sock.recv_calls == 3);
    assert(!atomic_load(&gate.pending));

    // A producer that queues after the drain leaves a readable datagram and
    // marks the gate pending for the next request.
    sock.race_at_call = 0;
    ++sock.datagrams;
    assert(wake_gate_request(&gate));
    wake_gate_drain(&gate, fake_recv, &sock);
    assert(!sock.datagrams && !atomic_load(&gate.pending));
}

static void test_send_failure_retries(void)
{
    wake_gate_t gate = { 0 };
    fake_socket_t sock = { .gate = &gate };
    assert(wake_gate_request(&gate));
    wake_gate_clear(&gate); // send failed: the next request must try again
    assert(wake_gate_request(&gate));
    assert(sock.datagrams == 0);
    ++sock.datagrams;
    wake_gate_drain(&gate, fake_recv, &sock);
    assert(!atomic_load(&gate.pending));
}

int main(void)
{
    test_clears_only_after_drain();
    test_race_during_drain_keeps_wakeup();
    test_send_failure_retries();
    puts("PASS wake gate drains before clearing and never strands a request");
    return 0;
}
