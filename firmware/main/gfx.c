#include "gfx.h"

#include <string.h>

#include "frame.h"

static uint16_t wire_color(uint32_t rgb)
{
    uint16_t value = ((rgb >> 8) & 0xf800) | ((rgb >> 5) & 0x07e0) | ((rgb >> 3) & 0x001f);
    const uint8_t bytes[] = { value >> 8, value & 0xff };
    uint16_t pixel;
    memcpy(&pixel, bytes, sizeof(pixel));
    return pixel;
}

static void rect(uint16_t *fb, int x, int y, int w, int h, uint16_t color)
{
    for (int row = y; row < y + h; ++row) {
        if (row < 0 || row >= FRAME_HEIGHT) continue;
        for (int col = x; col < x + w; ++col) {
            if (col >= 0 && col < FRAME_WIDTH) fb[row * FRAME_WIDTH + col] = color;
        }
    }
}

// 12x10 pixel cloud/sleepy icon.
static const char *const offline_icon[10] = {
    "....kkkk....",
    "..kkmmmmkk..",
    ".kmmmmmmmmk.",
    "kmmkmmmmkmmk",
    "kmmmmmmmmmmk",
    "kmmmmkkmmmmk",
    ".kmmmmmmmmk.",
    "..kkkkkkkk..",
    "....k..k....",
    "............",
};

// 5x7 bitmap glyphs for "offline" (o, f, l, i, n, e).
static const uint8_t offline_glyphs[6][7] = {
    { 0x0e, 0x11, 0x11, 0x11, 0x11, 0x11, 0x0e }, // o
    { 0x06, 0x09, 0x08, 0x1e, 0x08, 0x08, 0x08 }, // f
    { 0x0c, 0x04, 0x04, 0x04, 0x04, 0x04, 0x0e }, // l
    { 0x04, 0x00, 0x0c, 0x04, 0x04, 0x04, 0x0e }, // i
    { 0x00, 0x00, 0x16, 0x19, 0x11, 0x11, 0x11 }, // n
    { 0x00, 0x00, 0x0e, 0x11, 0x1f, 0x10, 0x0e }, // e
};

static void draw_glyph(uint16_t *fb, int x, int y, int scale, int idx, uint16_t color)
{
    for (int r = 0; r < 7; ++r) {
        uint8_t bits = offline_glyphs[idx][r];
        for (int c = 0; c < 5; ++c) {
            if (bits & (1 << (4 - c))) rect(fb, x + c * scale, y + r * scale, scale, scale, color);
        }
    }
}

void gfx_offline(uint16_t *fb)
{
    uint16_t bg = wire_color(0x1e1a2b);
    uint16_t ink = wire_color(0xfaf0f6);
    uint16_t accent = wire_color(0x9ae5c3);
    uint16_t outline = wire_color(0x705f7c);

    rect(fb, 0, 0, FRAME_WIDTH, FRAME_HEIGHT, bg);

    int scale = FRAME_WIDTH > 320 ? 6 : 4;
    int icon_w = 12 * scale;
    int icon_h = 10 * scale;
    int text_scale = FRAME_WIDTH > 320 ? 4 : 3;
    int text_w = (7 * 6 - 1) * text_scale;
    int text_h = 7 * text_scale;
    int gap = 3 * scale;
    int total_h = icon_h + gap + text_h;

    int ix = (FRAME_WIDTH - icon_w) / 2;
    int iy = (FRAME_HEIGHT - total_h) / 2;
    for (int r = 0; r < 10; ++r) {
        for (int c = 0; c < 12; ++c) {
            char ch = offline_icon[r][c];
            if (ch == 'k') rect(fb, ix + c * scale, iy + r * scale, scale, scale, outline);
            else if (ch == 'm') rect(fb, ix + c * scale, iy + r * scale, scale, scale, accent);
        }
    }

    static const int word[7] = { 0, 1, 1, 2, 3, 4, 5 }; // "offline"
    int tx = (FRAME_WIDTH - text_w) / 2;
    int ty = iy + icon_h + gap;
    for (int i = 0; i < 7; ++i) {
        draw_glyph(fb, tx + i * 6 * text_scale, ty, text_scale, word[i], ink);
    }
}
