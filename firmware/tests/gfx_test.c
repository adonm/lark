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

int main(void)
{
    gfx_offline_cat(fb);
    // Background #1E1A2B -> RGB565 0x18C5
    expect_pixel(0, 0, 0x18, 0xc5);
    expect_pixel(FRAME_WIDTH - 1, FRAME_HEIGHT - 1, 0x18, 0xc5);
    // Center of icon should contain accent (#9AE5C3 -> 0x9F38) or outline (#705F7C -> 0x72EF)
    int found_icon = 0;
    for (int y = FRAME_HEIGHT / 4; y < FRAME_HEIGHT / 2; ++y) {
        const uint8_t *bytes = (const uint8_t *)&fb[y * FRAME_WIDTH + FRAME_WIDTH / 2];
        if (bytes[0] == 0x9f && bytes[1] == 0x38) {
            found_icon = 1;
            break;
        }
    }
    assert(found_icon);
    puts("PASS offline cat fallback in LCD byte order");
}
