#pragma once

// Pure reconnect policy for net_client.c.
enum {
    NET_BACKOFF_MIN_MS = 1000,
    NET_BACKOFF_MAX_MS = 30000,
    NET_BACKOFF_STEP_AT_MS = 16000,
};

// Doubles the delay, jumps to the cap from 16 s, and never exceeds it.
int net_backoff_next(int current_ms);

// A successful connection always restarts at the minimum delay.
int net_backoff_reset(void);
