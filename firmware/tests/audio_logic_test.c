#include "audio_logic.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static void test_record_limits_and_stale_reads(void)
{
    assert(audio_record_frame_limit(false) == 960000u);  // 60 s at 16 kHz
    assert(audio_record_frame_limit(true) == 4880000u);  // 305 s listen window
    assert(!audio_stale_read_is_fresh(2999));
    assert(audio_stale_read_is_fresh(3000));
    assert(audio_stale_read_is_fresh(9000));
}

static void test_fade_and_abort_tail(void)
{
    audio_fade_plan_t normal = audio_fade_plan(false, 1234);
    assert(normal.fade && normal.tail_frames == AUDIO_DMA_TAIL_FRAMES);

    audio_fade_plan_t silent = audio_fade_plan(false, 0);
    assert(!silent.fade && silent.tail_frames == AUDIO_DMA_TAIL_FRAMES);

    // An abort skips the fade but still flushes the BSP queue.
    audio_fade_plan_t abort = audio_fade_plan(true, 1234);
    assert(!abort.fade && abort.tail_frames == AUDIO_DMA_TAIL_FRAMES);
}

static void test_mix_fade_chime_and_clamp(void)
{
    assert(audio_mix_sample(1000, 0, false, 0) == 0);
    assert(audio_mix_sample(1000, AUDIO_FADE_SAMPLES / 2, false, 0) == 500);
    assert(audio_mix_sample(1000, AUDIO_FADE_SAMPLES, false, 0) == 1000);
    assert(audio_mix_sample(1000, AUDIO_FADE_SAMPLES + 1, false, 0) == 1000);

    assert(audio_mix_sample(1000, AUDIO_FADE_SAMPLES, true, 500) == 1500);
    assert(audio_mix_sample(32000, AUDIO_FADE_SAMPLES, true, 1000) == 32767);
    assert(audio_mix_sample(-32000, AUDIO_FADE_SAMPLES, true, -1000) == -32768);
}

typedef struct {
    uint8_t bytes[64];
    size_t len;
    int calls;
    size_t fail_after;
} capture_t;

static bool capture_sink(const void *data, size_t len, void *ctx)
{
    capture_t *c = ctx;
    if (c->fail_after && (size_t)c->calls >= c->fail_after) return false;
    ++c->calls;
    assert(c->len + len <= sizeof(c->bytes));
    memcpy(c->bytes + c->len, data, len);
    c->len += len;
    return true;
}

static void test_tts_carry_across_chunks(void)
{
    audio_tts_splitter_t split = { 0 };
    capture_t c = { 0 };
    const uint8_t a[] = { 1, 2, 3 };
    const uint8_t b[] = { 4, 5, 6, 7 };
    const uint8_t d[] = { 8 };
    assert(audio_tts_feed(&split, a, sizeof(a), capture_sink, &c));
    assert(c.len == 2 && c.bytes[0] == 1 && c.bytes[1] == 2);
    assert(split.odd && split.carry == 3);
    assert(audio_tts_feed(&split, b, sizeof(b), capture_sink, &c));
    assert(audio_tts_feed(&split, d, sizeof(d), capture_sink, &c));
    assert(!split.odd);
    const uint8_t want[] = { 1, 2, 3, 4, 5, 6, 7, 8 };
    assert(c.len == sizeof(want) && memcmp(c.bytes, want, sizeof(want)) == 0);

    // A rejected sink stops the stream.
    audio_tts_splitter_t fail = { 0 };
    capture_t stop = { .fail_after = 1 };
    const uint8_t chunk[] = { 1, 2, 3, 4 };
    assert(audio_tts_feed(&fail, chunk, 2, capture_sink, &stop));
    assert(!audio_tts_feed(&fail, chunk, sizeof(chunk), capture_sink, &stop));
    // Empty input never calls the sink.
    capture_t empty = { 0 };
    assert(audio_tts_feed(&split, chunk, 0, capture_sink, &empty));
    assert(empty.calls == 0);
}

int main(void)
{
    test_record_limits_and_stale_reads();
    test_fade_and_abort_tail();
    test_mix_fade_chime_and_clamp();
    test_tts_carry_across_chunks();
    puts("PASS audio record limits, abort tail, fade mixing and TTS carry");
    return 0;
}
