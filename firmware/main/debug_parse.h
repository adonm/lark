#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

// Minimal readers for the handful of tiny JSON bodies the device accepts.
// Every reader rejects trailing text, truncation and overflow.

static inline const char *debug_skip_ws(const char *p)
{
    while (*p == ' ' || *p == '\t' || *p == '\r' || *p == '\n') ++p;
    return p;
}

// Reads an unsigned decimal no greater than max, then leaves *pp after it.
static inline bool debug_parse_uint(const char **pp, uint32_t max, uint32_t *out)
{
    const char *p = debug_skip_ws(*pp);
    if (*p < '0' || *p > '9') return false;
    uint32_t v = 0;
    do {
        uint32_t digit = (uint32_t)(*p++ - '0');
        if (v > (max - digit) / 10) return false;
        v = v * 10 + digit;
    } while (*p >= '0' && *p <= '9');
    *pp = p;
    *out = v;
    return true;
}

static inline bool debug_parse_u16(const char **pp, uint16_t *out)
{
    uint32_t v = 0;
    if (!debug_parse_uint(pp, 65535, &v)) return false;
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

// {"level":0..100}
static inline bool debug_parse_level(const char *s, int *out)
{
    if (!s || !out) return false;
    const char *p = debug_skip_ws(s);
    if (*p++ != '{') return false;
    p = debug_skip_ws(p);
    if (strncmp(p, "\"level\"", 7) != 0) return false;
    p = debug_skip_ws(p + 7);
    if (*p++ != ':') return false;
    uint32_t level = 0;
    if (!debug_parse_uint(&p, 100, &level)) return false;
    p = debug_skip_ws(p);
    if (*p++ != '}') return false;
    p = debug_skip_ws(p);
    if (*p != '\0') return false;
    *out = (int)level;
    return true;
}

static inline bool debug_mic_len_valid(size_t len, size_t max_bytes)
{
    return len > 0 && len <= max_bytes && (len % 2) == 0;
}

// Committed /debug/mic PCM state. The generation invalidates an upload that a
// newer request (or an aborted read) superseded; len only advances on commit.
typedef struct {
    size_t len, pos;
    uint32_t gen;
} debug_mic_state_t;

static inline uint32_t debug_mic_invalidate(debug_mic_state_t *s)
{
    s->len = 0;
    s->pos = 0;
    if (++s->gen == 0) ++s->gen;
    return s->gen;
}

static inline bool debug_mic_load_ok(debug_mic_state_t *s, uint32_t gen, size_t offset, size_t len,
                                     size_t total, size_t max_bytes, bool commit)
{
    if (!s || gen != s->gen || total > max_bytes || (total % 2) != 0) return false;
    if (offset > total || len > total - offset) return false;
    if (offset == 0) {
        // A new body must never expose the previous committed buffer.
        s->len = 0;
        s->pos = 0;
    }
    if (commit) {
        s->len = total;
        s->pos = 0;
    }
    return true;
}

static inline void debug_mic_abort(debug_mic_state_t *s, uint32_t gen)
{
    if (!s || gen != s->gen) return;
    s->len = 0;
    s->pos = 0;
    if (++s->gen == 0) ++s->gen;
}
