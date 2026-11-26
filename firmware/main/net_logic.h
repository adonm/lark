#pragma once

#include <stdbool.h>
#include <stdint.h>

// Pure reconnect, recording-upload and TTS-ack policy for net_client.c.

enum {
    NET_BACKOFF_MIN_MS = 1000,
    NET_BACKOFF_MAX_MS = 30000,
    NET_BACKOFF_STEP_AT_MS = 16000,
    // 50 consecutive 20 ms RTP failures is the audio time a capture may lose
    // before it is treated as dead rather than merely congested.
    NET_UPLOAD_FAIL_LIMIT = 50,
};

// Backoff is always bounded. A bad token or protocol version keeps retrying,
// because a server-side misconfiguration can heal without a board reboot.
typedef struct {
    int delay_ms;
} net_backoff_t;

void net_backoff_init(net_backoff_t *backoff);

// Doubles the delay, jumps to the cap from 16 s, and never exceeds it.
void net_backoff_failed(net_backoff_t *backoff);

// Only a WELCOME proves the token and protocol were accepted.
void net_backoff_connected(net_backoff_t *backoff);

// Any reject, including a terminal one, backs off like a failure, capped at
// NET_BACKOFF_MAX_MS.
void net_backoff_rejected(net_backoff_t *backoff);

int net_backoff_delay_ms(const net_backoff_t *backoff);

typedef struct {
    unsigned failures;
} net_upload_t;

typedef enum {
    NET_UPLOAD_SENT,     // the datagram reached the socket
    NET_UPLOAD_DROPPED,  // transient failure: drop this packet, keep recording
    NET_UPLOAD_CANCELLED, // too many consecutive failures: end the capture
} net_upload_action_t;

void net_upload_init(net_upload_t *upload);
net_upload_action_t net_upload_result(net_upload_t *upload, bool sent, unsigned fail_limit);

// A single pending acknowledgement: the newest stream replaces an unsent one,
// and an abort never overwrites a completed ack for the same audio id.
typedef struct {
    bool pending;
    bool aborted;
    uint32_t session;
    uint32_t audio_id;
} net_ack_t;

void net_ack_submit(net_ack_t *ack, uint32_t session, uint32_t audio_id, bool aborted);

// Copies the pending ack. The caller clears it with net_ack_sent after the
// frame is on the wire.
bool net_ack_take(const net_ack_t *ack, net_ack_t *out);
void net_ack_sent(net_ack_t *ack);
