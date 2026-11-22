#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

static inline const char *debug_skip_ws(const char *p)
{
    while (*p == ' ' || *p == '\t' || *p == '\r' || *p == '\n') ++p;
    return p;
}

static inline bool debug_parse_u16(const char **pp, uint16_t *out)
{
    const char *p = debug_skip_ws(*pp);
    if (*p < '0' || *p > '9') return false;
    uint32_t v = 0;
    while (*p >= '0' && *p <= '9') {
        v = v * 10 + (uint32_t)(*p++ - '0');
        if (v > 65535) return false;
    }
    *pp = p;
    *out = (uint16_t)v;
    return true;
}

static inline bool debug_parse_tap(const char *s, uint16_t width, uint16_t height, uint16_t *out_x, uint16_t *out_y)
{
    if (!s || !out_x || !out_y) return false;
    const char *p = debug_skip_ws(s);
    if (*p++ != '{') return false;
    bool got_x = false, got_y = false;
    uint16_t x = 0, y = 0;
    for (int i = 0; i < 2; ++i) {
        p = debug_skip_ws(p);
        if (*p++ != '"') return false;
        char key = *p++;
        if ((key != 'x' && key != 'y') || *p++ != '"') return false;
        p = debug_skip_ws(p);
        if (*p++ != ':') return false;
        uint16_t val = 0;
        if (!debug_parse_u16(&p, &val)) return false;
        if (key == 'x') {
            if (got_x) return false;
            got_x = true;
            x = val;
        } else {
            if (got_y) return false;
            got_y = true;
            y = val;
        }
        p = debug_skip_ws(p);
        if (i == 0) {
            if (*p++ != ',') return false;
        }
    }
    if (!got_x || !got_y || *p++ != '}') return false;
    p = debug_skip_ws(p);
    if (*p != '\0') return false;
    if (x >= width || y >= height) return false;
    *out_x = x;
    *out_y = y;
    return true;
}

static inline bool debug_mic_len_valid(size_t len, size_t max_bytes)
{
    return len > 0 && len <= max_bytes && (len % 2) == 0;
}
