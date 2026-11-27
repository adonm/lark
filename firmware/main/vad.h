#pragma once

#include <stdbool.h>
#include <stdint.h>

// One update per 10 ms with the mean squared amplitude of the mono block.
// After speech, send after 150 silent blocks, or 300 once voiced reaches 400.
typedef struct {
    uint32_t quiet[4];
    uint32_t threshold;
    unsigned frames, streak, voiced, silence;
} vad_t;

typedef enum { VAD_CONTINUE, VAD_SEND, VAD_NO_SPEECH } vad_result_t;
vad_result_t vad_update(vad_t *vad, uint32_t energy);
bool vad_edge_watchdog(vad_t *vad, uint32_t energy);
