#include <assert.h>
#include <stdio.h>
#include "debug_parse.h"

int main(void)
{
    uint16_t x = 0, y = 0;
    assert(debug_parse_tap("{\"x\":180,\"y\":320}", 360, 360, &x, &y) && x == 180 && y == 320);
    assert(debug_parse_tap(" { \"y\" : 239 , \"x\" : 319 } \n", 320, 240, &x, &y) && x == 319 && y == 239);
    assert(!debug_parse_tap("{\"x\":320,\"y\":0}", 320, 240, &x, &y));
    assert(!debug_parse_tap("{\"x\":0,\"y\":240}", 320, 240, &x, &y));
    assert(!debug_parse_tap("{\"x\":-1,\"y\":0}", 320, 240, &x, &y));
    assert(!debug_parse_tap("{\"x\":1,\"x\":2}", 320, 240, &x, &y));
    assert(!debug_parse_tap("{\"x\":1}", 320, 240, &x, &y));
    assert(!debug_parse_tap("{\"x\":1,\"y\":2} extra", 320, 240, &x, &y));

    assert(!debug_mic_len_valid(0, 320000));
    assert(!debug_mic_len_valid(3, 320000));
    assert(!debug_mic_len_valid(320002, 320000));
    assert(debug_mic_len_valid(320000, 320000));
    assert(debug_mic_len_valid(640, 320000));

    printf("PASS host debug_parse tests\n");
    return 0;
}
