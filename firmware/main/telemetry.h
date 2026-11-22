#pragma once

#include <inttypes.h>
#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
    size_t free_heap, min_free_heap, free_psram;
    uint64_t uptime_s;
    int wifi_rssi;
    unsigned cpu_pct;
    uint32_t last_frame_rev, display_flush_ms, input_drops, audio_underruns, audio_drops;
} telemetry_t;

extern struct telemetry_io {
    atomic_uint last_frame_rev, display_flush_ms, input_drops, audio_underruns, audio_drops;
} telemetry_io;

#define TELEMETRY_IO_JSON ",\"last_frame_rev\":%" PRIu32 ",\"display_flush_ms\":%" PRIu32 \
    ",\"input_drops\":%" PRIu32 ",\"audio_underruns\":%" PRIu32 ",\"audio_drops\":%" PRIu32
#define TELEMETRY_IO_VALUES(hw) (hw).last_frame_rev, (hw).display_flush_ms, (hw).input_drops, \
    (hw).audio_underruns, (hw).audio_drops

telemetry_t telemetry_read(void);
