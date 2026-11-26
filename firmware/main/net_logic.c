#include "net_logic.h"

#include <stddef.h>

void net_backoff_init(net_backoff_t *backoff)
{
    if (!backoff) return;
    backoff->delay_ms = NET_BACKOFF_MIN_MS;
}

void net_backoff_failed(net_backoff_t *backoff)
{
    if (!backoff) return;
    int current = backoff->delay_ms < NET_BACKOFF_MIN_MS ? NET_BACKOFF_MIN_MS : backoff->delay_ms;
    if (current >= NET_BACKOFF_MAX_MS || current > NET_BACKOFF_STEP_AT_MS) {
        backoff->delay_ms = NET_BACKOFF_MAX_MS;
        return;
    }
    int next = current * 2;
    backoff->delay_ms = next > NET_BACKOFF_MAX_MS ? NET_BACKOFF_MAX_MS : next;
}

void net_backoff_connected(net_backoff_t *backoff)
{
    if (!backoff) return;
    backoff->delay_ms = NET_BACKOFF_MIN_MS;
}

void net_backoff_rejected(net_backoff_t *backoff)
{
    net_backoff_failed(backoff);
}

int net_backoff_delay_ms(const net_backoff_t *backoff)
{
    return backoff ? backoff->delay_ms : NET_BACKOFF_MIN_MS;
}

void net_upload_init(net_upload_t *upload)
{
    if (upload) upload->failures = 0;
}

net_upload_action_t net_upload_result(net_upload_t *upload, bool sent, unsigned fail_limit)
{
    if (!upload) return NET_UPLOAD_CANCELLED;
    if (sent) {
        upload->failures = 0;
        return NET_UPLOAD_SENT;
    }
    ++upload->failures;
    return upload->failures >= fail_limit ? NET_UPLOAD_CANCELLED : NET_UPLOAD_DROPPED;
}

void net_ack_submit(net_ack_t *ack, uint32_t session, uint32_t audio_id, bool aborted)
{
    if (!ack || !session || !audio_id) return;
    // A completion already queued for this stream is final; a late abort from
    // the interrupt path must not erase it.
    if (ack->pending && ack->session == session && ack->audio_id == audio_id && !ack->aborted) return;
    ack->pending = true;
    ack->aborted = aborted;
    ack->session = session;
    ack->audio_id = audio_id;
}

bool net_ack_take(const net_ack_t *ack, net_ack_t *out)
{
    if (!ack || !out || !ack->pending) return false;
    *out = *ack;
    return true;
}

void net_ack_sent(net_ack_t *ack)
{
    if (ack) ack->pending = false;
}
