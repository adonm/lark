#include "frame.h"

#include <limits.h>
#include <string.h>

#ifdef ESP_PLATFORM
#include "esp_heap_caps.h"
#include "esp_jpeg_dec.h"
#include "lz4.h"
#endif

enum { FRAME_SCRATCH_CAP = (FRAME_WIDTH + 16) * (FRAME_HEIGHT + 16) * 2 };

void frame_union(frame_bounds_t *dirty, frame_rect_t r)
{
    if (!r.w || !r.h || r.x + r.w > FRAME_WIDTH || r.y + r.h > FRAME_HEIGHT) return;
    // Neither panel requires even coordinates, but an even RGB565 width keeps
    // all SPI DMA strips word-sized, including the final single-row strip.
    _Static_assert(FRAME_WIDTH % 2 == 0, "LCD width must fit aligned RGB565 strips");
    int x0 = r.x & ~1, x1 = (r.x + r.w + 1) & ~1;
    int y0 = r.y, y1 = r.y + r.h;
    bool empty = dirty->x0 == dirty->x1;
    if (empty || x0 < dirty->x0) dirty->x0 = x0;
    if (empty || y0 < dirty->y0) dirty->y0 = y0;
    if (empty || x1 > dirty->x1) dirty->x1 = x1;
    if (empty || y1 > dirty->y1) dirty->y1 = y1;
}

bool frame_copy_strip(uint16_t *dst, const uint16_t *fb, frame_bounds_t area, int y, int rows)
{
    if (!dst || !fb || rows <= 0 || area.x0 < 0 || area.x0 >= area.x1 || area.x1 > FRAME_WIDTH ||
        area.y0 < 0 || y < area.y0 || y >= area.y1 || rows > area.y1 - y || area.y1 > FRAME_HEIGHT) {
        return false;
    }
    size_t width = (size_t)(area.x1 - area.x0);
    if (area.x0 == 0 && area.x1 == FRAME_WIDTH) {
        memcpy(dst, fb + (size_t)y * FRAME_WIDTH, (size_t)rows * FRAME_WIDTH * sizeof(*dst));
        return true;
    }
    for (int row = 0; row < rows; ++row) {
        memcpy(dst + (size_t)row * width, fb + ((size_t)y + row) * FRAME_WIDTH + area.x0, width * sizeof(*dst));
    }
    return true;
}

static uint16_t le16(const uint8_t *p)
{
    return (uint16_t)p[0] | ((uint16_t)p[1] << 8);
}

#ifndef ESP_PLATFORM
static uint16_t be16(const uint8_t *p)
{
    return ((uint16_t)p[0] << 8) | (uint16_t)p[1];
}
#endif

static uint8_t *frame_scratch(size_t needed)
{
    if (needed > FRAME_SCRATCH_CAP) return NULL;
#ifdef ESP_PLATFORM
    static uint8_t *scratch;
    if (!scratch) {
        scratch = heap_caps_aligned_alloc(16, FRAME_SCRATCH_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
        if (!scratch) {
            scratch = heap_caps_aligned_alloc(16, FRAME_SCRATCH_CAP, MALLOC_CAP_8BIT);
        }
    }
    return scratch;
#else
    static _Alignas(16) uint8_t scratch[FRAME_SCRATCH_CAP];
    return scratch;
#endif
}

static bool rle_valid(const uint8_t *p, size_t len, size_t pixels)
{
    while (pixels && len) {
        uint8_t tag = *p++;
        --len;
        size_t run = (tag & 0x7f) + 1;
        size_t bytes = (tag & 0x80) ? 2 : run * 2;
        if (run > pixels || bytes > len) return false;
        p += bytes;
        len -= bytes;
        pixels -= run;
    }
    return pixels == 0 && len == 0;
}

static void decode_rle(uint16_t *out, const uint8_t *p, size_t pixels)
{
    size_t pos = 0;
    while (pos < pixels) {
        uint8_t tag = *p++;
        size_t run = (tag & 0x7f) + 1;
        if (tag & 0x80) {
            uint16_t pixel;
            // Copy the representation, not a numeric RGB565 value.
            memcpy(&pixel, p, 2);
            p += 2;
            for (size_t i = 0; i < run; ++i) out[pos++] = pixel;
        } else {
            memcpy(out + pos, p, run * 2);
            p += run * 2;
            pos += run;
        }
    }
}

#ifndef ESP_PLATFORM
// Host-only test doubles for the vendor decoders used on device; see frame.h.
static bool lz4_block_valid(const uint8_t *src, size_t src_len, size_t expected_dst)
{
    const uint8_t *ip = src, *iend = src + src_len;
    size_t dst_pos = 0;
    while (ip < iend) {
        uint8_t token = *ip++;
        size_t lit_len = token >> 4;
        if (lit_len == 15) {
            uint8_t s;
            do {
                if (ip >= iend) return false;
                s = *ip++;
                if (lit_len > SIZE_MAX - s) return false;
                lit_len += s;
            } while (s == 255);
        }
        if (lit_len > (size_t)(iend - ip) || lit_len > expected_dst - dst_pos) return false;
        ip += lit_len;
        dst_pos += lit_len;
        if (ip == iend) break;
        if ((size_t)(iend - ip) < 2) return false;
        uint16_t offset = le16(ip);
        ip += 2;
        if (offset == 0 || offset > dst_pos) return false;
        size_t match_len = (token & 0x0f) + 4;
        if ((token & 0x0f) == 15) {
            uint8_t s;
            do {
                if (ip >= iend) return false;
                s = *ip++;
                if (match_len > SIZE_MAX - s) return false;
                match_len += s;
            } while (s == 255);
        }
        if (match_len > expected_dst - dst_pos) return false;
        dst_pos += match_len;
    }
    return dst_pos == expected_dst && ip == iend;
}

static int lz4_block_decode(const uint8_t *src, size_t src_len, uint8_t *dst, size_t dst_cap)
{
    const uint8_t *ip = src, *iend = src + src_len;
    uint8_t *op = dst, *oend = dst + dst_cap;
    while (ip < iend) {
        uint8_t token = *ip++;
        size_t lit_len = token >> 4;
        if (lit_len == 15) {
            uint8_t s;
            do {
                if (ip >= iend) return -1;
                s = *ip++;
                if (lit_len > SIZE_MAX - s) return -1;
                lit_len += s;
            } while (s == 255);
        }
        if (lit_len > (size_t)(iend - ip) || lit_len > (size_t)(oend - op)) return -1;
        memcpy(op, ip, lit_len);
        ip += lit_len;
        op += lit_len;
        if (ip == iend) break;
        if ((size_t)(iend - ip) < 2) return -1;
        uint16_t offset = le16(ip);
        ip += 2;
        if (offset == 0 || offset > (size_t)(op - dst)) return -1;
        size_t match_len = (token & 0x0f) + 4;
        if ((token & 0x0f) == 15) {
            uint8_t s;
            do {
                if (ip >= iend) return -1;
                s = *ip++;
                if (match_len > SIZE_MAX - s) return -1;
                match_len += s;
            } while (s == 255);
        }
        if (match_len > (size_t)(oend - op)) return -1;
        const uint8_t *match = op - offset;
        for (size_t i = 0; i < match_len; ++i) *op++ = match[i];
    }
    return (int)(op - dst);
}

static bool jpeg_valid(const uint8_t *src, size_t len, uint16_t w, uint16_t h)
{
    if (len < 4 || src[0] != 0xff || src[1] != 0xd8 || src[len - 2] != 0xff || src[len - 1] != 0xd9) {
        return false;
    }
    bool has_sof0 = false, has_sos = false;
    size_t pos = 2;
    while (pos < len) {
        if (src[pos] != 0xff) return false;
        while (pos < len && src[pos] == 0xff) ++pos;
        if (pos >= len) return false;
        uint8_t marker = src[pos++];
        if (marker == 0xd9) {
            return has_sof0 && has_sos && pos == len;
        }
        if (marker == 0x00 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7)) {
            continue;
        }
        if (pos + 2 > len) return false;
        uint16_t seg_len = be16(src + pos);
        if (seg_len < 2 || pos + seg_len > len) return false;
        if (marker == 0xc0) { // SOF0 Baseline DCT
            if (seg_len < 7 || src[pos + 2] == 0) return false;
            uint16_t jh = be16(src + pos + 3);
            uint16_t jw = be16(src + pos + 5);
            if (jw != w || jh != h) return false;
            has_sof0 = true;
        } else if (marker == 0xc2) { // SOF2 Progressive DCT not supported by esp_new_jpeg
            return false;
        } else if (marker == 0xda) { // SOS: scan entropy data until EOI
            has_sos = true;
            return has_sof0;
        }
        pos += seg_len;
    }
    return false;
}
#endif

static const uint8_t *decode_lz4(const uint8_t *payload, size_t len, frame_rect_t r)
{
    size_t expected = (size_t)r.w * r.h * 2;
    if (len > (size_t)INT_MAX || expected > (size_t)INT_MAX) return NULL;
    uint8_t *dst = frame_scratch(expected);
    if (!dst) return NULL;

#ifdef ESP_PLATFORM
    int dec = LZ4_decompress_safe((const char *)payload, (char *)dst, (int)len, (int)expected);
#else
    if (!lz4_block_valid(payload, len, expected)) return NULL;
    int dec = lz4_block_decode(payload, len, dst, expected);
#endif
    return dec == (int)expected ? dst : NULL;
}

static bool decode_jpeg(const uint8_t *payload, size_t len, frame_rect_t r, const uint8_t **out_pixels)
{
#ifdef ESP_PLATFORM
    if (len < 4 || len > (size_t)INT_MAX || payload[0] != 0xff || payload[1] != 0xd8) return false;
    static jpeg_dec_handle_t jpeg_dec;
    if (!jpeg_dec) {
        jpeg_dec_config_t config = DEFAULT_JPEG_DEC_CONFIG();
        config.output_type = JPEG_PIXEL_FORMAT_RGB565_BE;
        if (jpeg_dec_open(&config, &jpeg_dec) != JPEG_ERR_OK) {
            jpeg_dec = NULL;
            return false;
        }
    }
    jpeg_dec_io_t io = {
        .inbuf = (uint8_t *)payload,
        .inbuf_len = (int)len,
    };
    jpeg_dec_header_info_t info = { 0 };
    if (jpeg_dec_parse_header(jpeg_dec, &io, &info) != JPEG_ERR_OK) return false;
    if (info.width != r.w || info.height != r.h) return false;
    int outbuf_len = 0;
    if (jpeg_dec_get_outbuf_len(jpeg_dec, &outbuf_len) != JPEG_ERR_OK || outbuf_len <= 0) return false;
    uint8_t *out = frame_scratch((size_t)outbuf_len);
    if (!out) return false;
    io.outbuf = out;
    io.out_size = outbuf_len;
    if (jpeg_dec_process(jpeg_dec, &io) != JPEG_ERR_OK) return false;
    *out_pixels = out;
    return true;
#else
    *out_pixels = NULL;
    return jpeg_valid(payload, len, r.w, r.h);
#endif
}

bool frame_decode(const uint8_t *data, size_t len, frame_decoded_t *out)
{
    if (!data || !out || len < FRAME_HEADER_SIZE || data[0] != FRAME_TAG) return false;
    frame_rect_t r = {
        .x = le16(data + 1), .y = le16(data + 3), .w = le16(data + 5),
        .h = le16(data + 7), .rev_lo = le16(data + 10),
    };
    if (!r.w || !r.h || r.x + r.w > FRAME_WIDTH || r.y + r.h > FRAME_HEIGHT) return false;
    const uint8_t *payload = data + FRAME_HEADER_SIZE;
    len -= FRAME_HEADER_SIZE;
    size_t pixels = (size_t)r.w * r.h;
    const uint8_t *decoded = NULL;
    switch (data[9]) {
    case FRAME_CODEC_RAW:
        if (len != pixels * 2) return false;
        decoded = payload;
        break;
    case FRAME_CODEC_RLE565: {
        if (!rle_valid(payload, len, pixels)) return false;
        uint8_t *scratch = frame_scratch(pixels * 2);
        if (!scratch) return false;
        decode_rle((uint16_t *)scratch, payload, pixels);
        decoded = scratch;
        break;
    }
    case FRAME_CODEC_JPEG:
        if (!decode_jpeg(payload, len, r, &decoded)) return false;
        break;
    case FRAME_CODEC_LZ4:
        decoded = decode_lz4(payload, len, r);
        if (!decoded) return false;
        break;
    default:
        return false;
    }
    out->rect = r;
    out->pixels = decoded;
    return true;
}

void frame_commit(uint16_t *fb, const frame_decoded_t *decoded)
{
    if (!fb || !decoded || !decoded->pixels) return;
    frame_rect_t r = decoded->rect;
    size_t row_bytes = (size_t)r.w * 2;
    if (r.x == 0 && r.w == FRAME_WIDTH) {
        memcpy(fb + (size_t)r.y * FRAME_WIDTH, decoded->pixels, (size_t)r.h * row_bytes);
    } else {
        for (size_t row = 0; row < r.h; ++row) {
            memcpy(fb + (r.y + row) * FRAME_WIDTH + r.x, decoded->pixels + row * row_bytes, row_bytes);
        }
    }
}

bool frame_apply(uint16_t *fb, const uint8_t *data, size_t len, frame_rect_t *dirty)
{
    if (!fb || !dirty) return false;
    frame_decoded_t decoded;
    if (!frame_decode(data, len, &decoded)) return false;
    frame_commit(fb, &decoded);
    *dirty = decoded.rect;
    return true;
}
