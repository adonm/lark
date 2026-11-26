#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdatomic.h>

#include "lwip/sockets.h"

// Socket ownership, TCP framing, the wake pipe and the RTP uplink. Session
// state (handshake, queues, retries) lives in net_client.c.

bool net_transport_resolve(const char *host, uint16_t port, struct sockaddr_in *remote);
bool net_transport_open(const struct sockaddr_in *remote, uint16_t *local_udp_port);
void net_transport_close(void);
int net_transport_tcp_fd(void);
int net_transport_wake_fd(void);

// Packs and sends one control frame. A failed or short write marks the stream
// broken so the caller reconnects instead of writing another frame.
bool net_transport_send(uint8_t type, const void *payload, uint32_t len);
bool net_transport_recv_exact(uint8_t *dst, size_t len, const atomic_bool *online);

bool net_transport_broken(void);
void net_transport_clear_broken(void);

void net_transport_init_wake(void);
// Pokes the blocked service select; a no-op when the wake pipe is unavailable.
void net_transport_kick(void);
void net_transport_drain_wake(void);

void net_transport_set_udp_peer(uint16_t port, uint32_t ssrc);
void net_transport_clear_udp_peer(void);
int16_t *net_transport_rtp_buf(void);
bool net_transport_send_rtp(uint8_t pt, const int16_t *pcm, size_t samples);
