#include "frame.h"
#include "gfx.h"

#include <assert.h>
#include <stdio.h>

static uint16_t fb[FRAME_PIXELS];

static void expect_pixel(unsigned x, unsigned y, uint8_t hi, uint8_t lo)
{
    const uint8_t *bytes = (const uint8_t *)&fb[y * FRAME_WIDTH + x];
    assert(bytes[0] == hi && bytes[1] == lo);
}

static void expect_art(unsigned x, unsigned y, uint8_t hi, uint8_t lo)
{
#ifdef LARK_BOX3
    x = 40 + x * 2 / 3;
    y = y * 2 / 3;
#endif
    expect_pixel(x, y, hi, lo);
}

int main(void)
{
    gfx_offline_cat(fb);
    expect_pixel(0, 0, 0xff, 0x9a);
    expect_pixel(FRAME_WIDTH - 1, FRAME_HEIGHT - 1, 0xff, 0x9a);
    expect_art(180, 77, 0xf4, 0xe1);
    expect_art(112, 142, 0x18, 0xc4);
    expect_art(162, 207, 0x18, 0xc4);
    expect_art(248, 142, 0x18, 0xc4);
    puts("PASS offline cat fallback in LCD byte order");
}
