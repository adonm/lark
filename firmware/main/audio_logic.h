#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

// Pure recording/playback arithmetic, extracted from audio.c so host tests can
// cover it without the ESP-IDF codec.

enum {
    AUDIO_FADE_SAMPLES = 80,       // 5 ms at 16 kHz
    AUDIO_DMA_TAIL_FRAMES = 6 * 240, // BSP TX queue, in samples
    AUDIO_STALE_READ_US = 3000,
    AUDIO_STALE_READ_TRIES = 12,
};

typedef struct {
    bool fade;          // write one fade-in/fade-out frame first
    size_t tail_frames; // zero/silence frames written after it
} audio_fade_plan_t;

// A stale RX DMA frame is one that took at least 3 ms to read.
bool audio_stale_read_is_fresh(int64_t elapsed_us);

// 60 s normally, the longer listen window when requested.
size_t audio_record_frame_limit(bool listen);

// An abort skips the fade but must still flush the queued DMA tail.
audio_fade_plan_t audio_fade_plan(bool stopped, int16_t last);

// Fade in a chunk of speech and optionally overlay one chime sample.
int16_t audio_mix_sample(int32_t sample, uint32_t played_samples, bool chime, int16_t chime_sample);

// Reassembles 16-bit TTS PCM across arbitrary TCP chunk boundaries and hands
// whole even chunks to the sink. Returns false when the sink rejected part of
// the stream.
typedef struct {
    uint8_t carry;
    bool odd;
} audio_tts_splitter_t;

typedef bool (*audio_tts_sink_t)(const void *data, size_t len, void *ctx);

bool audio_tts_feed(audio_tts_splitter_t *split, const uint8_t *data, size_t len,
                    audio_tts_sink_t sink, void *ctx);
