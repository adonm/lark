#include "net_logic.h"

int net_backoff_next(int current_ms)
{
    if (current_ms >= NET_BACKOFF_MAX_MS) return NET_BACKOFF_MAX_MS;
    if (current_ms > NET_BACKOFF_STEP_AT_MS) return NET_BACKOFF_MAX_MS;
    if (current_ms < NET_BACKOFF_MIN_MS) current_ms = NET_BACKOFF_MIN_MS;
    int next = current_ms * 2;
    return next > NET_BACKOFF_MAX_MS ? NET_BACKOFF_MAX_MS : next;
}

int net_backoff_reset(void)
{
    return NET_BACKOFF_MIN_MS;
}
