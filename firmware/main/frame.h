#pragma once

#include "board.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

enum { FRAME_PIXELS = FRAME_WIDTH * FRAME_HEIGHT };
enum { FRAME_TAG = 0x03, FRAME_HEADER_SIZE = 12, FRAME_PACKET_CAP = FRAME_HEADER_SIZE + FRAME_PIXELS * 2 };
enum {
    FRAME_CODEC_RAW = 0,
    FRAME_CODEC_RLE565 = 1,
    FRAME_CODEC_JPEG = 2,
    FRAME_CODEC_LZ4 = 3,
};

typedef struct {
    uint16_t x, y, w, h, rev_lo;
} frame_rect_t;

typedef struct {
    int x0, y0, x1, y1; // Half-open LCD bounds. Zero initialized means empty.
} frame_bounds_t;

void frame_union(frame_bounds_t *dirty, frame_rect_t rect);

// Pixels are stored in LCD wire byte order. Invalid packets leave fb and dirty untouched.
bool frame_apply(uint16_t *fb, const uint8_t *data, size_t len, frame_rect_t *dirty);
