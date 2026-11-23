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

typedef struct {
    frame_rect_t rect;
    const uint8_t *pixels; // Raw payload or internal scratch; valid until next frame_decode.
} frame_decoded_t;

void frame_union(frame_bounds_t *dirty, frame_rect_t rect);

// Packs rows [y, y+rows) of the dirty area into a strip for SPI DMA.
bool frame_copy_strip(uint16_t *dst, const uint16_t *fb, frame_bounds_t area, int y, int rows);

// Pixels are stored in LCD wire byte order. Invalid packets leave fb and dirty untouched.
// Host tests compile a local LZ4 block decoder and JPEG header validator instead
// of the shipped decoders: espressif/lz4 and esp_new_jpeg are managed components
// fetched at device build time and are not available to the host harness. Those
// host doubles check framing and malformed input; they do not exercise the
// vendor decode path. A change to codec semantics needs a device build.
bool frame_decode(const uint8_t *data, size_t len, frame_decoded_t *out);
void frame_commit(uint16_t *fb, const frame_decoded_t *decoded);
bool frame_apply(uint16_t *fb, const uint8_t *data, size_t len, frame_rect_t *dirty);
