#include <assert.h>
#include <stdio.h>
#include "debug_parse.h"

static void test_tap(void)
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
}

static void test_unsigned_readers(void)
{
    uint32_t v = 0;
    const char *p;
    p = "65535x";
    assert(debug_parse_uint(&p, 65535, &v) && v == 65535 && *p == 'x');
    p = "65536";
    assert(!debug_parse_uint(&p, 65535, &v));
    p = "999999999999999999999999";
    assert(!debug_parse_uint(&p, 65535, &v));
    p = "";
    assert(!debug_parse_uint(&p, 65535, &v));
    p = " 42  7";
    assert(debug_parse_uint(&p, 100, &v) && v == 42 && *p == ' ');
}

static void test_level(void)
{
    int level = -1;
    assert(debug_parse_level("{\"level\":0}", &level) && level == 0);
    assert(debug_parse_level(" { \"level\" : 100 } \n", &level) && level == 100);
    assert(!debug_parse_level("{\"level\":101}", &level));
    assert(!debug_parse_level("{\"level\":99999999999999999999}", &level));
    assert(!debug_parse_level("{\"level\":-1}", &level));
    assert(!debug_parse_level("{\"level\":50,\"x\":1}", &level));
    assert(!debug_parse_level("{\"Level\":50}", &level));
    assert(!debug_parse_level("{\"level\":}", &level));
    assert(!debug_parse_level("{\"level\":50", &level));
    assert(!debug_parse_level("{\"level\":50} trailing", &level));
}

static void test_debug_mic_generations(void)
{
    assert(!debug_mic_len_valid(0, 320000));
    assert(!debug_mic_len_valid(3, 320000));
    assert(!debug_mic_len_valid(320002, 320000));
    assert(debug_mic_len_valid(320000, 320000));
    assert(debug_mic_len_valid(640, 320000));

    debug_mic_state_t s = { 0 };
    uint32_t first = debug_mic_invalidate(&s);
    assert(first == 1 && s.len == 0);
    // An aborted transfer never becomes readable.
    assert(debug_mic_load_ok(&s, first, 0, 320, 1000, 320000, false));
    assert(s.len == 0);
    debug_mic_abort(&s, first);
    assert(s.len == 0 && s.gen != first);

    // Chunks accumulate, and only the final commit exposes the buffer.
    uint32_t second = debug_mic_invalidate(&s);
    assert(debug_mic_load_ok(&s, second, 0, 320, 640, 320000, false));
    assert(debug_mic_load_ok(&s, second, 320, 320, 640, 320000, true));
    assert(s.len == 640 && s.pos == 0);

    // Offset 0 invalidates the previous commit before the next body commits.
    uint32_t third = debug_mic_invalidate(&s);
    assert(s.len == 0);
    assert(debug_mic_load_ok(&s, third, 0, 640, 640, 320000, false));
    assert(s.len == 0);

    // An older upload cannot commit over a superseding generation.
    assert(!debug_mic_load_ok(&s, second, 0, 640, 640, 320000, true));
    assert(!debug_mic_load_ok(&s, third, 600, 100, 640, 320000, true));
    assert(!debug_mic_load_ok(&s, third, 0, 640, 641, 320000, true));
    assert(!debug_mic_load_ok(&s, third, 0, 640, 320002, 320000, true));

    // Aborts from a stale handler leave the current generation alone.
    debug_mic_abort(&s, second);
    assert(debug_mic_load_ok(&s, third, 0, 640, 640, 320000, true));
    assert(s.len == 640);
}

int main(void)
{
    test_tap();
    test_unsigned_readers();
    test_level();
    test_debug_mic_generations();
    printf("PASS host debug_parse tests\n");
    return 0;
}
