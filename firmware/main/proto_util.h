#pragma once

// Hand-written envelope, RTP and URL helpers included at the end of the
// generated proto.h. Message structs and codecs are generated.

static inline void lark_pack_hdr(uint8_t *dst, uint8_t type, uint16_t seq, uint32_t length)
{
    dst[0] = LARK_PROTO_MAGIC;
    dst[1] = type;
    dst[2] = (uint8_t)(seq & 0xFF);
    dst[3] = (uint8_t)(seq >> 8);
    dst[4] = (uint8_t)(length & 0xFF);
    dst[5] = (uint8_t)((length >> 8) & 0xFF);
    dst[6] = (uint8_t)((length >> 16) & 0xFF);
    dst[7] = (uint8_t)((length >> 24) & 0xFF);
}

static inline bool lark_unpack_hdr(const uint8_t *src, lark_hdr_t *out)
{
    if (!src || !out || src[0] != LARK_PROTO_MAGIC) return false;
    uint32_t length = (uint32_t)src[4] | ((uint32_t)src[5] << 8) |
                      ((uint32_t)src[6] << 16) | ((uint32_t)src[7] << 24);
    if (length > LARK_PROTO_MAX_PAYLOAD) return false;
    out->magic = src[0];
    out->type = src[1];
    out->seq = (uint16_t)src[2] | ((uint16_t)src[3] << 8);
    out->length = length;
    return true;
}

static inline void lark_rtp_encode(uint8_t *dst, uint8_t pt, uint16_t seq, uint32_t ts, uint32_t ssrc)
{
    dst[0] = LARK_RTP_VERSION_BYTE;
    dst[1] = pt & 0x7F;
    dst[2] = (uint8_t)(seq >> 8);
    dst[3] = (uint8_t)(seq & 0xFF);
    dst[4] = (uint8_t)(ts >> 24);
    dst[5] = (uint8_t)((ts >> 16) & 0xFF);
    dst[6] = (uint8_t)((ts >> 8) & 0xFF);
    dst[7] = (uint8_t)(ts & 0xFF);
    dst[8] = (uint8_t)(ssrc >> 24);
    dst[9] = (uint8_t)((ssrc >> 16) & 0xFF);
    dst[10] = (uint8_t)((ssrc >> 8) & 0xFF);
    dst[11] = (uint8_t)(ssrc & 0xFF);
}

static inline bool lark_rtp_decode(const uint8_t *src, size_t len, uint8_t *pt, uint16_t *seq, uint32_t *ts, uint32_t *ssrc)
{
    if (!src || len < LARK_RTP_HDR_SIZE || src[0] != LARK_RTP_VERSION_BYTE) return false;
    if (pt) *pt = src[1] & 0x7F;
    if (seq) *seq = ((uint16_t)src[2] << 8) | (uint16_t)src[3];
    if (ts) *ts = ((uint32_t)src[4] << 24) | ((uint32_t)src[5] << 16) | ((uint32_t)src[6] << 8) | (uint32_t)src[7];
    if (ssrc) *ssrc = ((uint32_t)src[8] << 24) | ((uint32_t)src[9] << 16) | ((uint32_t)src[10] << 8) | (uint32_t)src[11];
    return true;
}

static inline bool lark_parse_edge_url(const char *url, char *host_out, size_t host_cap, uint16_t *port_out)
{
    if (!url || !host_out || !host_cap || !port_out) return false;
    const char *p = NULL;
    bool custom_port = false;
    if (!strncmp(url, "http://", 7)) {
        p = url + 7;
    } else if (!strncmp(url, "lark://", 7)) {
        p = url + 7;
        custom_port = true;
    } else {
        if (strstr(url, "://")) return false;
        p = url;
        custom_port = true;
    }
    size_t auth_len = strcspn(p, "/?#");
    if (!auth_len) return false;
    for (size_t i = 0; i < auth_len; ++i) {
        if (p[i] == '@') return false;
    }
    const char *host_start = p;
    size_t host_len = auth_len;
    uint16_t port = LARK_DEFAULT_DEVICE_PORT;
    if (p[0] == '[') {
        const char *close = memchr(p, ']', auth_len);
        if (!close || close == p + 1) return false;
        host_start = p + 1;
        host_len = (size_t)(close - host_start);
        const char *after = close + 1;
        if (after < p + auth_len) {
            if (*after != ':') return false;
            const char *port_str = after + 1;
            size_t plen = (size_t)((p + auth_len) - port_str);
            if (!plen) return false;
            char *end = NULL;
            unsigned long v = strtoul(port_str, &end, 10);
            if (end != port_str + plen || v == 0 || v > 65535) return false;
            if (custom_port) port = (uint16_t)v;
        }
    } else {
        const char *colon = memchr(p, ':', auth_len);
        if (colon) {
            if (memchr(colon + 1, ':', (size_t)((p + auth_len) - (colon + 1)))) return false;
            host_len = (size_t)(colon - p);
            const char *port_str = colon + 1;
            size_t plen = (size_t)((p + auth_len) - port_str);
            if (!host_len || !plen) return false;
            char *end = NULL;
            unsigned long v = strtoul(port_str, &end, 10);
            if (end != port_str + plen || v == 0 || v > 65535) return false;
            if (custom_port) port = (uint16_t)v;
        }
    }
    if (!host_len || host_len >= host_cap) return false;
    memcpy(host_out, host_start, host_len);
    host_out[host_len] = '\0';
    *port_out = port;
    return true;
}
