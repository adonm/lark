#include "frame.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static uint16_t fb[FRAME_PIXELS];
static uint8_t raw[FRAME_HEADER_SIZE + FRAME_PIXELS * 2];
enum { TEST_REV = 0xab3e, SENTINEL = 0xa5 };

static void header(uint8_t *packet, unsigned x, unsigned y, unsigned w, unsigned h, uint8_t codec)
{
    const uint8_t bytes[] = {
        FRAME_TAG, x & 0xff, x >> 8, y & 0xff, y >> 8,
        w & 0xff, w >> 8, h & 0xff, h >> 8, codec, TEST_REV & 0xff, TEST_REV >> 8,
    };
    memcpy(packet, bytes, sizeof(bytes));
}

static void apply(const uint8_t *data, size_t len, unsigned x, unsigned y, unsigned w, unsigned h)
{
    frame_rect_t dirty;
    assert(frame_apply(fb, data, len, &dirty));
    assert(dirty.x == x && dirty.y == y && dirty.w == w && dirty.h == h && dirty.rev_lo == TEST_REV);
}

static void expect_rect(unsigned x, unsigned y, unsigned w, unsigned h, const uint8_t *pixels)
{
    const uint8_t *bytes = (const uint8_t *)fb;
    for (unsigned row = 0; row < FRAME_HEIGHT; ++row) {
        for (unsigned col = 0; col < FRAME_WIDTH; ++col) {
            const uint8_t *actual = bytes + (row * FRAME_WIDTH + col) * 2;
            if (row >= y && row < y + h && col >= x && col < x + w) {
                assert(memcmp(actual, pixels + ((row - y) * w + col - x) * 2, 2) == 0);
            } else {
                assert(actual[0] == SENTINEL && actual[1] == SENTINEL);
            }
        }
    }
}

static void test_raw(void)
{
    header(raw, 0, 0, FRAME_WIDTH, FRAME_HEIGHT, 0);
    for (unsigned i = 0; i < FRAME_PIXELS; ++i) {
        raw[FRAME_HEADER_SIZE + i * 2] = (uint8_t)(i >> 8);
        raw[FRAME_HEADER_SIZE + i * 2 + 1] = (uint8_t)i;
    }
    apply(raw, sizeof(raw), 0, 0, FRAME_WIDTH, FRAME_HEIGHT);
    assert(memcmp(fb, raw + FRAME_HEADER_SIZE, sizeof(fb)) == 0);

    const uint8_t pixels[] = { 0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f, 0xde, 0xad, 0xbe, 0xef, 0xff, 0xff };
    uint8_t packet[FRAME_HEADER_SIZE + sizeof(pixels)];
    header(packet, FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2, 0);
    memcpy(packet + FRAME_HEADER_SIZE, pixels, sizeof(pixels));
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, sizeof(packet), FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2);
    expect_rect(FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2, pixels);
}

static void test_rle(void)
{
    const uint8_t literal[] = { 0x02, 0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f };
    uint8_t packet[FRAME_HEADER_SIZE + 1 + 128 * 2 + 3 + 3 + 3];
    header(packet, 10, 12, 1, 3, 1);
    memcpy(packet + FRAME_HEADER_SIZE, literal, sizeof(literal));
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, FRAME_HEADER_SIZE + sizeof(literal), 10, 12, 1, 3);
    expect_rect(10, 12, 1, 3, literal + 1);

    const uint8_t repeat[] = { 0x82, 0x00, 0x1f };
    const uint8_t blues[] = { 0x00, 0x1f, 0x00, 0x1f, 0x00, 0x1f };
    memcpy(packet + FRAME_HEADER_SIZE, repeat, sizeof(repeat));
    apply(packet, FRAME_HEADER_SIZE + sizeof(repeat), 10, 12, 1, 3);
    expect_rect(10, 12, 1, 3, blues);

    // Both maximum-length runs cross non-contiguous rows near the framebuffer edge.
    header(packet, FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6, 1);
    uint8_t pixels[43 * 6 * 2];
    for (unsigned i = 0; i < 128; ++i) {
        pixels[i * 2] = (uint8_t)(255 - i);
        pixels[i * 2 + 1] = (uint8_t)i;
        pixels[256 + i * 2] = 0xf8;
        pixels[256 + i * 2 + 1] = 0;
    }
    packet[FRAME_HEADER_SIZE] = 0x7f;
    memcpy(packet + FRAME_HEADER_SIZE + 1, pixels, 256);
    const uint8_t tail[] = { 0xff, 0xf8, 0, 0x00, 0x07, 0xe0, 0x80, 0x00, 0x1f };
    memcpy(packet + FRAME_HEADER_SIZE + 257, tail, sizeof(tail));
    memcpy(pixels + 512, tail + 4, 2);
    memcpy(pixels + 514, tail + 7, 2);
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, sizeof(packet), FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6);
    expect_rect(FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6, pixels);
}

static void reject(const uint8_t *data, size_t len)
{
    // Allocate exactly the reported length so ASan can catch physical overreads too.
    uint8_t *packet = malloc(len ? len : 1);
    assert(packet);
    memcpy(packet, data, len);
    frame_rect_t dirty = { .x = 1, .y = 2, .w = 3, .h = 4, .rev_lo = 5 };
    assert(!frame_apply(fb, packet, len, &dirty));
    assert(dirty.x == 1 && dirty.y == 2 && dirty.w == 3 && dirty.h == 4 && dirty.rev_lo == 5);
    expect_rect(0, 0, 0, 0, NULL);
    free(packet);
}

static void test_malformed(void)
{
    uint8_t packet[FRAME_HEADER_SIZE + 9] = { 0 };
    memset(fb, SENTINEL, sizeof(fb));
    const struct { unsigned x, y, w, h; uint8_t codec; } invalid[] = {
        { FRAME_WIDTH, 0, 1, 1, 0 }, { 0, FRAME_HEIGHT, 1, 1, 0 },
        { 0, 0, 0, 1, 0 }, { 0, 0, 1, 0, 0 },
        { FRAME_WIDTH - 1, 0, 2, 1, 1 }, { 0, FRAME_HEIGHT - 1, 1, 2, 1 },
        { 0, 0, FRAME_WIDTH + 1, 1, 0 }, { 0, 0, 1, FRAME_HEIGHT + 1, 0 },
        { 65535, 65535, 65535, 65535, 0 }, { 0, 0, 1, 1, FRAME_CODEC_JPEG }, { 0, 0, 1, 1, 4 },
    };
    for (size_t i = 0; i < sizeof(invalid) / sizeof(invalid[0]); ++i) {
        header(packet, invalid[i].x, invalid[i].y, invalid[i].w, invalid[i].h, invalid[i].codec);
        reject(packet, FRAME_HEADER_SIZE + 2);
    }

    header(packet, FRAME_WIDTH - 1, FRAME_HEIGHT - 1, 1, 1, 0);
    packet[0] = 0x02;
    reject(packet, FRAME_HEADER_SIZE + 2);
    packet[0] = FRAME_TAG;
    reject(packet, FRAME_HEADER_SIZE + 1);
    reject(packet, FRAME_HEADER_SIZE + 3);

    header(packet, FRAME_WIDTH - 2, FRAME_HEIGHT - 2, 2, 2, 1);
    uint8_t mixed[] = { 0x01, 0x12, 0x34, 0x56, 0x78, 0x81, 0x9a, 0xbc };
    memcpy(packet + FRAME_HEADER_SIZE, mixed, sizeof(mixed));
    for (size_t len = 0; len < FRAME_HEADER_SIZE + sizeof(mixed); ++len) reject(packet, len);
    reject(packet, sizeof(packet)); // trailing byte
    packet[FRAME_HEADER_SIZE + 5] = 0x82;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // repeat overruns rectangle after literal run
    packet[FRAME_HEADER_SIZE + 5] = 0x80;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // complete runs underfill rectangle
    packet[FRAME_HEADER_SIZE] = 0x7f;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // overlarge literal run
    packet[FRAME_HEADER_SIZE] = 0xff;
    reject(packet, FRAME_HEADER_SIZE + 3); // overlarge repeat at framebuffer edge

    frame_rect_t dirty;
    assert(!frame_apply(NULL, packet, sizeof(packet), &dirty));
    assert(!frame_apply(fb, NULL, sizeof(packet), &dirty));
    assert(!frame_apply(fb, packet, sizeof(packet), NULL));
}

static void test_lz4(void)
{
    // Full-width fast path (x == 0, w == FRAME_WIDTH) with overlapping match copy.
    enum { FULL_H = 4, FULL_BYTES = FRAME_WIDTH * FULL_H * 2 };
    size_t match_len = FULL_BYTES - 4;
    size_t match_extra = (match_len - 4 - 15) / 255 + 1;
    uint8_t *full_pkt = malloc(FRAME_HEADER_SIZE + 1 + 4 + 2 + match_extra);
    assert(full_pkt);
    header(full_pkt, 0, 10, FRAME_WIDTH, FULL_H, FRAME_CODEC_LZ4);
    size_t p = FRAME_HEADER_SIZE;
    full_pkt[p++] = 0x4f; // 4 literals, match_len >= 19
    full_pkt[p++] = 0x12;
    full_pkt[p++] = 0x34;
    full_pkt[p++] = 0x56;
    full_pkt[p++] = 0x78;
    full_pkt[p++] = 0x04; // offset = 4
    full_pkt[p++] = 0x00;
    size_t rem = match_len - 4 - 15;
    while (rem >= 255) {
        full_pkt[p++] = 255;
        rem -= 255;
    }
    full_pkt[p++] = (uint8_t)rem;

    uint8_t full_expected[FULL_BYTES];
    for (size_t i = 0; i < FULL_BYTES; i += 4) {
        full_expected[i] = 0x12;
        full_expected[i + 1] = 0x34;
        full_expected[i + 2] = 0x56;
        full_expected[i + 3] = 0x78;
    }
    memset(fb, SENTINEL, sizeof(fb));
    apply(full_pkt, p, 0, 10, FRAME_WIDTH, FULL_H);
    expect_rect(0, 10, FRAME_WIDTH, FULL_H, full_expected);
    free(full_pkt);

    // Sub-rect path (w < FRAME_WIDTH) with literals + match + trailing literals.
    // 12x5 = 60 pixels = 120 bytes: 6 literals + 100 match (offset 6) + 14 trailing literals.
    uint8_t sub_pkt[FRAME_HEADER_SIZE + 1 + 6 + 2 + 1 + 1 + 14];
    header(sub_pkt, 17, 13, 12, 5, FRAME_CODEC_LZ4);
    size_t sp = FRAME_HEADER_SIZE;
    sub_pkt[sp++] = 0x6f; // 6 literals, match_len = 15 + 4 + 81 = 100
    for (int i = 0; i < 6; ++i) sub_pkt[sp++] = (uint8_t)(0x10 + i);
    sub_pkt[sp++] = 0x06; // offset = 6
    sub_pkt[sp++] = 0x00;
    sub_pkt[sp++] = 81;   // 19 + 81 = 100
    sub_pkt[sp++] = 0xe0; // 14 trailing literals
    for (int i = 0; i < 14; ++i) sub_pkt[sp++] = (uint8_t)(0xa0 + i);

    uint8_t sub_expected[120];
    for (int i = 0; i < 106; ++i) sub_expected[i] = (uint8_t)(0x10 + (i % 6));
    for (int i = 0; i < 14; ++i) sub_expected[106 + i] = (uint8_t)(0xa0 + i);
    memset(fb, SENTINEL, sizeof(fb));
    apply(sub_pkt, sp, 17, 13, 12, 5);
    expect_rect(17, 13, 12, 5, sub_expected);

    // Malformed LZ4 (full-width and sub-rect) must leave fb untouched.
    memset(fb, SENTINEL, sizeof(fb));
    reject(sub_pkt, sp - 1); // truncated trailing literal
    uint8_t bad_offset[FRAME_HEADER_SIZE + 7] = { 0 };
    header(bad_offset, 0, 0, FRAME_WIDTH, 1, FRAME_CODEC_LZ4);
    bad_offset[FRAME_HEADER_SIZE] = 0x40; // 4 literals, match 4
    bad_offset[FRAME_HEADER_SIZE + 5] = 0x05; // offset 5 > dst_pos(4)
    bad_offset[FRAME_HEADER_SIZE + 6] = 0x00;
    reject(bad_offset, sizeof(bad_offset));
    bad_offset[FRAME_HEADER_SIZE + 5] = 0x00; // offset 0
    reject(bad_offset, sizeof(bad_offset));
}

static void test_jpeg(void)
{
    // Minimal Baseline JPEG (SOI + SOF0 64x64 + SOS + EOI).
    const uint8_t jpeg[] = {
        0xff, 0xd8,
        0xff, 0xc0, 0x00, 0x0b, 0x08, 0x00, 0x40, 0x00, 0x40, 0x01, 0x01, 0x11, 0x00,
        0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00,
        0xaa, 0xbb,
        0xff, 0xd9,
    };
    uint8_t pkt[FRAME_HEADER_SIZE + sizeof(jpeg)];
    header(pkt, 0, 0, 64, 64, FRAME_CODEC_JPEG);
    memcpy(pkt + FRAME_HEADER_SIZE, jpeg, sizeof(jpeg));
    memset(fb, SENTINEL, sizeof(fb));
    apply(pkt, sizeof(pkt), 0, 0, 64, 64);

    // Reject wrong dimensions, progressive SOF2, missing EOI, missing SOI.
    memset(fb, SENTINEL, sizeof(fb));
    header(pkt, 0, 0, 63, 64, FRAME_CODEC_JPEG);
    reject(pkt, sizeof(pkt));
    header(pkt, 0, 0, 64, 64, FRAME_CODEC_JPEG);
    pkt[FRAME_HEADER_SIZE + 3] = 0xc2; // SOF2 progressive
    reject(pkt, sizeof(pkt));
    pkt[FRAME_HEADER_SIZE + 3] = 0xc0;
    reject(pkt, sizeof(pkt) - 2); // missing EOI
    pkt[FRAME_HEADER_SIZE] = 0x00; // bad SOI
    reject(pkt, sizeof(pkt));
}

static void test_dirty_union(void)
{
    frame_bounds_t dirty = { 0 };
    frame_union(&dirty, (frame_rect_t){ .x = 65535, .w = 1, .h = 1 });
    assert(dirty.x0 == dirty.x1);
    frame_union(&dirty, (frame_rect_t){ .x = 91, .y = 12, .w = 2, .h = 1 });
    assert(dirty.x0 == 90 && dirty.x1 == 94 && dirty.y0 == 12 && dirty.y1 == 13);
    frame_union(&dirty, (frame_rect_t){ .x = 92, .y = 11, .w = 1, .h = 5 });
    assert(dirty.x0 == 90 && dirty.x1 == 94 && dirty.y0 == 11 && dirty.y1 == 16);
    frame_union(&dirty, (frame_rect_t){ .x = FRAME_WIDTH - 1, .y = FRAME_HEIGHT - 1, .w = 1, .h = 1 });
    assert(dirty.x0 == 90 && dirty.x1 == FRAME_WIDTH && dirty.y0 == 11 && dirty.y1 == FRAME_HEIGHT);
    frame_union(&dirty, (frame_rect_t){ .x = 0, .y = 0, .w = FRAME_WIDTH, .h = FRAME_HEIGHT });
    assert(dirty.x0 == 0 && dirty.x1 == FRAME_WIDTH && dirty.y0 == 0 && dirty.y1 == FRAME_HEIGHT);
}

static void test_strip_copy(void)
{
    // A 120x160 rect (the edge's coarse frame grid) committed at an odd x
    // coordinate, then copied out strip by strip exactly like the painter.
    enum { ROWS = 24, W = 120, H = 160 };
    const unsigned x = FRAME_WIDTH - W - 3, y = FRAME_HEIGHT - H - 7;
    assert(x < FRAME_WIDTH && x + W <= FRAME_WIDTH && y + H <= FRAME_HEIGHT);
    header(raw, x, y, W, H, 0);
    for (unsigned i = 0; i < W * H; ++i) {
        raw[FRAME_HEADER_SIZE + i * 2] = (uint8_t)(i * 7);
        raw[FRAME_HEADER_SIZE + i * 2 + 1] = (uint8_t)(i * 13);
    }
    memset(fb, SENTINEL, sizeof(fb));
    frame_rect_t rect;
    assert(frame_apply(fb, raw, FRAME_HEADER_SIZE + W * H * 2, &rect));
    frame_bounds_t area = { 0 };
    frame_union(&area, rect);

    uint16_t *strip = malloc(ROWS * FRAME_WIDTH * sizeof(*strip));
    assert(strip);
    int width = area.x1 - area.x0;
    int copied = 0;
    for (int sy = area.y0; sy < area.y1; sy += ROWS) {
        int rows = area.y1 - sy < ROWS ? area.y1 - sy : ROWS;
        assert(frame_copy_strip(strip, fb, area, sy, rows));
        for (int row = 0; row < rows; ++row) {
            assert(memcmp(strip + row * width, fb + ((size_t)sy + row) * FRAME_WIDTH + area.x0,
                          (size_t)width * sizeof(*strip)) == 0);
        }
        copied += rows * width;
    }
    assert(copied == (area.y1 - area.y0) * width);
    assert(width >= W && area.y1 - area.y0 == H);
    free(strip);

    // Out-of-range requests are refused instead of reading outside the framebuffer.
    uint16_t small[ROWS * FRAME_WIDTH];
    frame_bounds_t whole = { 0, 0, FRAME_WIDTH, FRAME_HEIGHT };
    assert(frame_copy_strip(small, fb, whole, 0, ROWS));
    assert(frame_copy_strip(small, fb, (frame_bounds_t){ 0, 0, FRAME_WIDTH, 1 }, 0, 1));
    assert(!frame_copy_strip(NULL, fb, whole, 0, 1));
    assert(!frame_copy_strip(small, NULL, whole, 0, 1));
    assert(!frame_copy_strip(small, fb, whole, 0, 0));
    assert(!frame_copy_strip(small, fb, whole, FRAME_HEIGHT, 1));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ 0, 0, FRAME_WIDTH, ROWS }, 0, ROWS + 1));
    assert(!frame_copy_strip(small, fb, area, area.y0 - 1, 1));
    assert(!frame_copy_strip(small, fb, area, area.y1 - 1, 2));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ 2, 0, 2, FRAME_HEIGHT }, 0, 1));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ -2, 0, 2, FRAME_HEIGHT }, 0, 1));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ 0, 0, FRAME_WIDTH + 2, FRAME_HEIGHT }, 0, 1));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ 0, 0, FRAME_WIDTH, FRAME_HEIGHT + 2 }, 0, 1));
    assert(!frame_copy_strip(small, fb, (frame_bounds_t){ 0, -1, FRAME_WIDTH, FRAME_HEIGHT }, 0, 1));
}

int main(void)
{
    test_raw();
    puts("PASS raw RGB565 full frame and sub-rect, exact wire bytes");
    test_rle();
    puts("PASS RLE565 literal, repeat, mixed and 128-pixel runs, sub-rect isolation");
    test_lz4();
    puts("PASS LZ4 full-width fast path, sub-rect, and malformed rejection");
    test_jpeg();
    puts("PASS JPEG baseline validation and malformed rejection");
    test_malformed();
    puts("PASS malformed frames rejected without framebuffer or dirty-rect changes");
    test_dirty_union();
    puts("PASS dirty bounds union and word alignment");
    test_strip_copy();
    puts("PASS 120x160 commit and strip copy, out-of-range strips refused");
}
