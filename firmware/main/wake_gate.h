#pragma once

#include <stdatomic.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// One byte is sent to the loopback wake socket per service request. Requests
// coalesce while a datagram is in flight, so the flag must only be cleared
// after the socket has been drained: a request that races the drain leaves a
// readable datagram behind for the next select().
typedef struct {
    _Atomic bool pending;
} wake_gate_t;

typedef int (*wake_recv_fn)(void *ctx, void *buf, size_t len);

static inline bool wake_gate_request(wake_gate_t *gate)
{
    return !atomic_exchange_explicit(&gate->pending, true, memory_order_acq_rel);
}

static inline void wake_gate_clear(wake_gate_t *gate)
{
    atomic_store_explicit(&gate->pending, false, memory_order_release);
}

static inline void wake_gate_drain(wake_gate_t *gate, wake_recv_fn recv, void *ctx)
{
    uint8_t buf[16];
    while (recv(ctx, buf, sizeof(buf)) > 0) {}
    wake_gate_clear(gate);
}
