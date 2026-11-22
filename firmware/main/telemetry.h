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
    uint32_t rec_start_us, tts_first_us, ptr_send_us, frame_flush_us;
    uint32_t tts_rms, tts_samples;
} telemetry_t;

extern struct telemetry_io {
    atomic_uint last_frame_rev, display_flush_ms, input_drops, audio_underruns, audio_drops;
    atomic_uint rec_start_us, tts_first_us, ptr_send_us, frame_flush_us;
    atomic_uint tts_rms, tts_samples;
} telemetry_io;

#define TELEMETRY_IO_JSON ",\"last_frame_rev\":%" PRIu32 ",\"display_flush_ms\":%" PRIu32 \
    ",\"input_drops\":%" PRIu32 ",\"audio_underruns\":%" PRIu32 ",\"audio_drops\":%" PRIu32 \
    ",\"rec_start_us\":%" PRIu32 ",\"tts_first_us\":%" PRIu32 \
    ",\"ptr_send_us\":%" PRIu32 ",\"frame_flush_us\":%" PRIu32 \
    ",\"tts_rms\":%" PRIu32 ",\"tts_samples\":%" PRIu32
#define TELEMETRY_IO_VALUES(hw) (hw).last_frame_rev, (hw).display_flush_ms, (hw).input_drops, \
    (hw).audio_underruns, (hw).audio_drops, (hw).rec_start_us, (hw).tts_first_us, \
    (hw).ptr_send_us, (hw).frame_flush_us, (hw).tts_rms, (hw).tts_samples

telemetry_t telemetry_read(void);
