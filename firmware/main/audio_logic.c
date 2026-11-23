#include "audio_logic.h"

#include <limits.h>

bool audio_read_waited_for_data(int64_t elapsed_us)
{
    return elapsed_us >= AUDIO_STALE_READ_US;
}

size_t audio_record_frame_limit(bool listen)
{
    return (size_t)AUDIO_SAMPLE_RATE *
           (size_t)(listen ? AUDIO_LISTEN_MAX_SECONDS : AUDIO_RECORD_MAX_SECONDS);
}

audio_fade_plan_t audio_fade_plan(bool stopped, int16_t last)
{
    audio_fade_plan_t plan = { .fade = last != 0 && !stopped, .tail_frames = AUDIO_DMA_TAIL_FRAMES };
    return plan;
}

int16_t audio_mix_sample(int32_t sample, uint32_t played_samples, bool chime, int16_t chime_sample)
{
    if (played_samples < AUDIO_FADE_SAMPLES) {
        sample = sample * (int32_t)played_samples / AUDIO_FADE_SAMPLES;
    }
    if (chime) {
        sample += chime_sample;
    }
    if (sample > INT16_MAX) sample = INT16_MAX;
    if (sample < INT16_MIN) sample = INT16_MIN;
    return (int16_t)sample;
}

bool audio_tts_feed(audio_tts_splitter_t *split, const uint8_t *data, size_t len,
                    audio_tts_sink_t sink, void *ctx)
{
    if (!split || !sink || (len && !data)) return false;
    if (!len) return true;
    if (split->odd) {
        uint8_t sample[2] = { split->carry, data[0] };
        if (!sink(sample, sizeof(sample), ctx)) return false;
        ++data;
        --len;
        split->odd = false;
    }
    size_t even = len & ~(size_t)1;
    if (even && !sink(data, even, ctx)) return false;
    if (len != even) {
        split->carry = data[even];
        split->odd = true;
    }
    return true;
}
