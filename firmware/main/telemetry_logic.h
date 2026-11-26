#pragma once

#include <stdbool.h>
#include <stdint.h>

// Pure telemetry arithmetic, host-tested outside the ESP-IDF task APIs.

enum {
    TELEMETRY_STATUS_AWAKE_MS = 5000,
    TELEMETRY_STATUS_SLEEPING_MS = 15000,
};

// Busy percentage from idle-run-time deltas for every core. Rounds to the
// nearest percent and clamps to 0..100.
unsigned telemetry_cpu_pct(uint64_t capacity_us, uint64_t idle_us);

// Maps an esp_reset_reason() value to a stable status string. Unknown values
// become "other" so the JSON never carries a bare enum number.
const char *telemetry_reset_reason(int reason);

// Telemetry cadence: slower while the panel sleeps, well inside the edge's
// 75 s read deadline but cutting idle radio wakeups by three.
int telemetry_status_interval_ms(bool sleeping);

// Accumulates muted PCM gaps in whole milliseconds while a TTS stream plays.
typedef struct {
    int64_t gap_start_us;
    uint32_t total_ms;
} telemetry_gap_t;

// starving=true opens a gap; starving=false closes it and adds its duration.
void telemetry_gap_update(telemetry_gap_t *gap, int64_t now_us, bool starving);
void telemetry_gap_close(telemetry_gap_t *gap, int64_t now_us);
