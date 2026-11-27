#include <assert.h>
#include <stdio.h>
#include "vad.h"

static void blocks(vad_t *vad, unsigned count, uint32_t rms)
{
    while (count--) {
        assert(vad_update(vad, rms * rms) == VAD_CONTINUE);
    }
}

int main(void)
{
    vad_t quiet = { 0 };
    blocks(&quiet, 699, 180);
    assert(!quiet.voiced);
    assert(vad_update(&quiet, 180 * 180) == VAD_SEND);

    vad_t voice = { 0 };
    blocks(&voice, 20, 250);
    assert(voice.threshold == 390625);
    blocks(&voice, 50, 1000);
    uint32_t keep_alive = voice.threshold / 4;
    for (unsigned i = 0; i < 149; ++i) {
        assert(vad_update(&voice, keep_alive - 1) == VAD_CONTINUE);
    }
    assert(voice.silence == 149);
    assert(vad_update(&voice, keep_alive) == VAD_CONTINUE);
    assert(voice.silence == 0 && voice.voiced == 51);
    blocks(&voice, 149, 250);
    assert(vad_update(&voice, 250 * 250) == VAD_SEND);

    vad_t short_noise = { 0 };
    blocks(&short_noise, 20, 150);
    blocks(&short_noise, 2, 2500);
    blocks(&short_noise, 677, 150);
    assert(!short_noise.voiced);
    assert(vad_update(&short_noise, 150 * 150) == VAD_SEND);

    vad_t word = { 0 };
    blocks(&word, 20, 150);
    blocks(&word, 3, 900);
    assert(word.voiced == 3);
    blocks(&word, 149, 150);
    assert(vad_update(&word, 150 * 150) == VAD_SEND);

    vad_t boundary = { 0 };
    blocks(&boundary, 20, 100);
    blocks(&boundary, 399, 900);
    assert(boundary.voiced == 399);
    blocks(&boundary, 149, 100);
    vad_t short_utterance = boundary;
    assert(vad_update(&short_utterance, 100 * 100) == VAD_SEND);

    // A resumed frame crosses the boundary without counting the pause as voiced.
    blocks(&boundary, 1, 900);
    assert(boundary.voiced == 400 && boundary.silence == 0);
    blocks(&boundary, 299, 100);
    assert(vad_update(&boundary, 100 * 100) == VAD_SEND);
    puts("PASS VAD: 150/300-frame silence at 399/400 voiced frames, 3-frame word, /4 hysteresis, 700-frame quiet send");

    vad_t edge = { 0 };
    for (unsigned i = 0; i < 20 + 799; ++i) assert(!vad_edge_watchdog(&edge, 0));
    assert(vad_edge_watchdog(&edge, 0));
    edge = (vad_t){ 0 };
    for (unsigned i = 0; i < 20 + 700; ++i) assert(!vad_edge_watchdog(&edge, 0));
    for (unsigned i = 0; i < 3; ++i) assert(!vad_edge_watchdog(&edge, 1000 * 1000));
    for (unsigned i = 0; i < 799; ++i) assert(!vad_edge_watchdog(&edge, 0));
    assert(vad_edge_watchdog(&edge, 0));
    edge = (vad_t){ 0 };
    for (unsigned i = 0; i < 20; ++i) assert(!vad_edge_watchdog(&edge, 0));
    for (unsigned i = 20; i < 5999; ++i) assert(!vad_edge_watchdog(&edge, 1000 * 1000));
    assert(vad_edge_watchdog(&edge, 1000 * 1000));
    puts("PASS edge VAD watchdog: 8 s silence, speech resets silence, 60 s cap");
}
