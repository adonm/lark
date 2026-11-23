#pragma once

#include <stdbool.h>
#include <stdint.h>

// Pure telemetry arithmetic, host-tested outside the ESP-IDF task APIs.

// Busy percentage from idle-run-time deltas for every core. Rounds to the
// nearest percent and clamps to 0..100.
unsigned telemetry_cpu_pct(uint64_t capacity_us, uint64_t idle_us);

// Accumulates muted PCM gaps in whole milliseconds while a TTS stream plays.
typedef struct {
    int64_t gap_start_us;
    uint32_t total_ms;
} telemetry_gap_t;

// starving=true opens a gap; starving=false closes it and adds its duration.
void telemetry_gap_update(telemetry_gap_t *gap, int64_t now_us, bool starving);
void telemetry_gap_close(telemetry_gap_t *gap, int64_t now_us);
