#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum {
    LARK_PROTO_MAGIC = 0x4C, // 'L'
    LARK_PROTO_VERSION = 1,
    LARK_PROTO_HDR_SIZE = 8,
    LARK_PROTO_MAX_PAYLOAD = 320 * 1024,
    LARK_RESUME_TOKEN_LEN = 16,
    LARK_RESUME_WINDOW_MS = 30000,
    LARK_DEFAULT_DEVICE_PORT = 8766,
    LARK_DEFAULT_HTTP_PORT = 8765,
};

// Terminal -> Lark message types
enum {
    LARK_MSG_HELLO = 0x01,
    LARK_MSG_RESUME = 0x02,
    LARK_MSG_TELEMETRY = 0x03,
    LARK_MSG_POINTER = 0x04,
    LARK_MSG_WAKE_WORD = 0x05,
    LARK_MSG_REC_START = 0x06,
    LARK_MSG_REC_END = 0x07,
    LARK_MSG_TTS_ACK = 0x08,
    LARK_MSG_MEDIA = 0x0A,
    LARK_MSG_TEXT_INPUT = 0x0B,
    LARK_MSG_PROMPT_REPLY = 0x0C,
    LARK_MSG_WATCH = 0x0D,
    LARK_MSG_SET_VOLUME = 0x0E,
    LARK_MSG_PING = 0x0F,
    LARK_MSG_PONG = 0x10,
};

// Lark -> Terminal message types
enum {
    LARK_MSG_WELCOME = 0x81,
    LARK_MSG_REJECT = 0x82,
    LARK_MSG_FRAME = 0x83,
    LARK_MSG_FRAME_SVG = 0x84,
    LARK_MSG_WAKE_WORD_CTRL = 0x85,
    LARK_MSG_REC_CTRL = 0x86,
    LARK_MSG_TTS_CTRL = 0x87,
    LARK_MSG_TTS_PCM = 0x88,
    LARK_MSG_CHIME = 0x8A,
    LARK_MSG_POWER = 0x8B,
    LARK_MSG_VOLUME = 0x8C,
    LARK_MSG_STATUS = 0x8D,
    LARK_MSG_VOICE_OWNER = 0x8E,
    LARK_MSG_PROMPT = 0x8F,
    LARK_MSG_PROMPT_ENDED = 0x90,
    LARK_MSG_COMPANION = 0x91,
    LARK_MSG_PEERS = 0x92,
    LARK_MSG_EVENT = 0x93,
};

// Terminal kind
enum {
    LARK_KIND_HARDWARE = 0,
    LARK_KIND_WEB = 1,
};

// Display format bits
enum {
    LARK_DISP_RGB565 = 1u << 0,
    LARK_DISP_SVG = 1u << 1,
};

// Frame codec bits
enum {
    LARK_CODEC_RAW = 1u << 0,
    LARK_CODEC_RLE565 = 1u << 1,
    LARK_CODEC_LZ4 = 1u << 2,
    LARK_CODEC_JPEG = 1u << 3,
};

// Screen shape
enum {
    LARK_SHAPE_ROUND = 0,
    LARK_SHAPE_RECT = 1,
};

// Hello flags
enum {
    LARK_HELLO_FLAG_IF_FREE = 1u << 0,
};

// Capability bits in HELLO.caps_mask
enum {
    LARK_CAP_BATTERY = 1u << 0,
    LARK_CAP_HEAD_PADS = 1u << 1,
    LARK_CAP_DRAG = 1u << 4,
    LARK_CAP_POINTER_REV = 1u << 5,
    LARK_CAP_BRIDGE_ENDPOINT = 1u << 6,
    LARK_CAP_PANEL_SLEEP = 1u << 7,
    LARK_CAP_WAKE_WORD = 1u << 8,
};

// Reject reasons
enum {
    LARK_REJECT_BAD_TOKEN = 1,
    LARK_REJECT_BAD_VERSION = 2,
    LARK_REJECT_UNKNOWN_TERMINAL = 3,
    LARK_REJECT_FULL = 4,
    LARK_REJECT_OCCUPIED = 5,
    LARK_REJECT_BAD_CAPS = 6,
};

// Status states
enum {
    LARK_STATE_IDLE = 0,
    LARK_STATE_LISTENING = 1,
    LARK_STATE_THINKING = 2,
    LARK_STATE_SPEAKING = 3,
    LARK_STATE_BLOCKED = 4,
    LARK_STATE_DONE = 5,
    LARK_STATE_ERROR = 6,
};

// Prompt / status kinds
enum {
    LARK_PROMPT_NONE = 0,
    LARK_PROMPT_PERMISSION = 1,
    LARK_PROMPT_QUESTION = 2,
};

// Pointer kinds
enum {
    LARK_PTR_TAP = 0,
    LARK_PTR_DRAG = 1,
    LARK_PTR_UP = 2,
};

// Recording ctrl ops and target flags
enum {
    LARK_REC_STOP = 0,
    LARK_REC_START = 1,
    LARK_REC_CANCEL = 2,
    LARK_REC_TARGET_INTERCOM = 1u << 0,
    LARK_REC_TARGET_BRIDGE_EP = 1u << 7,
};

// TTS ctrl ops
enum {
    LARK_TTS_STOP = 0,
    LARK_TTS_START = 1,
    LARK_TTS_END = 2,
};

// Chime kinds
enum {
    LARK_CHIME_TAP = 0,
    LARK_CHIME_THINK = 1,
    LARK_CHIME_CANCEL = 2,
    LARK_CHIME_INTERCOM = 3,
    LARK_CHIME_RING = 4,
};

// Welcome flags
enum {
    LARK_WELCOME_FLAG_BRIDGE_EP = 1u << 0,
};

// Telemetry flags
enum {
    LARK_TELEMETRY_SLEEPING = 1u << 0,
};

// RTP (RFC 3550) constants for 20 ms 16 kHz mono 16-bit PCM
enum {
    LARK_RTP_VERSION_BYTE = 0x80,
    LARK_RTP_HDR_SIZE = 12,
    LARK_RTP_SAMPLES = 320,
    LARK_RTP_PCM_BYTES = LARK_RTP_SAMPLES * 2,
    LARK_RTP_PACKET_BYTES = LARK_RTP_HDR_SIZE + LARK_RTP_PCM_BYTES,
    LARK_RTP_PT_REC_PCM = 96,
};

typedef struct __attribute__((packed)) {
    uint8_t magic;
    uint8_t type;
    uint16_t seq;
    uint32_t length;
} lark_hdr_t;

typedef struct __attribute__((packed)) {
    uint8_t proto_version;
    uint8_t kind;
    uint32_t caps_mask;
    uint8_t display_formats;
    uint8_t frame_codecs;
    uint16_t width;
    uint16_t height;
    uint8_t shape;
    uint8_t flags;
    uint16_t udp_port;
    uint8_t id_len;
    uint8_t comp_len;
    uint8_t board_len;
    uint8_t fw_len;
    uint8_t tok_len;
} lark_hello_t;

typedef struct __attribute__((packed)) {
    uint8_t proto_version;
    uint8_t resume_token[LARK_RESUME_TOKEN_LEN];
    uint32_t last_seq;
    uint8_t id_len;
} lark_resume_t;

typedef struct __attribute__((packed)) {
    uint8_t proto_version;
    uint8_t flags;
    uint16_t udp_port;
    uint32_t ssrc;
    uint32_t max_msg_bytes;
    uint32_t resume_window_ms;
    uint32_t seq;
    uint8_t resume_token[LARK_RESUME_TOKEN_LEN];
    uint8_t id_len;
    uint8_t comp_len;
} lark_welcome_t;

typedef struct __attribute__((packed)) {
    uint8_t reason;
    uint8_t msg_len;
} lark_reject_t;

typedef struct __attribute__((packed)) {
    int16_t wifi_rssi;
    int16_t battery_pct;
    uint8_t charging;
    uint8_t cpu_pct;
    uint8_t flags;
    uint8_t pad;
    uint32_t free_heap;
    uint32_t uptime_s;
} lark_telemetry_t;

typedef struct __attribute__((packed)) {
    uint8_t kind;
    uint8_t pad;
    uint16_t x;
    uint16_t y;
    uint16_t rev_lo;
} lark_pointer_t;

typedef struct __attribute__((packed)) {
    uint8_t op;
    uint8_t flags;
    uint16_t session;
    uint8_t target_len;
} lark_rec_ctrl_t;

typedef struct __attribute__((packed)) {
    uint8_t op;
    uint64_t audio_id;
} lark_tts_ctrl_t;

typedef struct __attribute__((packed)) {
    uint8_t aborted;
    uint64_t audio_id;
} lark_tts_ack_t;

typedef struct __attribute__((packed)) {
    uint32_t seq;
    uint8_t state;
    uint8_t progress;
    uint8_t kind;
    uint16_t text_len;
} lark_status_t;

_Static_assert(sizeof(lark_hdr_t) == 8, "lark_hdr_t must be 8 bytes");
_Static_assert(sizeof(lark_hello_t) == 21, "lark_hello_t must be 21 bytes");
_Static_assert(sizeof(lark_resume_t) == 22, "lark_resume_t must be 22 bytes");
_Static_assert(sizeof(lark_welcome_t) == 38, "lark_welcome_t must be 38 bytes");
_Static_assert(sizeof(lark_reject_t) == 2, "lark_reject_t must be 2 bytes");
_Static_assert(sizeof(lark_telemetry_t) == 16, "lark_telemetry_t must be 16 bytes");
_Static_assert(sizeof(lark_pointer_t) == 8, "lark_pointer_t must be 8 bytes");
_Static_assert(sizeof(lark_rec_ctrl_t) == 5, "lark_rec_ctrl_t must be 5 bytes");
_Static_assert(sizeof(lark_tts_ctrl_t) == 9, "lark_tts_ctrl_t must be 9 bytes");
_Static_assert(sizeof(lark_tts_ack_t) == 9, "lark_tts_ack_t must be 9 bytes");
_Static_assert(sizeof(lark_status_t) == 9, "lark_status_t must be 9 bytes");

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

static inline size_t lark_encode_hello(uint8_t *dst, size_t cap, const lark_hello_t *h,
                                       const char *term_id, const char *comp_id,
                                       const char *board, const char *fw, const char *tok)
{
    if (!dst || !h) return 0;
    size_t total = sizeof(lark_hello_t) + h->id_len + h->comp_len + h->board_len + h->fw_len + h->tok_len;
    if (total > cap) return 0;
    memcpy(dst, h, sizeof(*h));
    size_t off = sizeof(*h);
    if (h->id_len && term_id) memcpy(dst + off, term_id, h->id_len);
    off += h->id_len;
    if (h->comp_len && comp_id) memcpy(dst + off, comp_id, h->comp_len);
    off += h->comp_len;
    if (h->board_len && board) memcpy(dst + off, board, h->board_len);
    off += h->board_len;
    if (h->fw_len && fw) memcpy(dst + off, fw, h->fw_len);
    off += h->fw_len;
    if (h->tok_len && tok) memcpy(dst + off, tok, h->tok_len);
    return total;
}

static inline bool lark_decode_welcome(const uint8_t *src, size_t len, lark_welcome_t *out,
                                       char *term_id, size_t term_cap,
                                       char *comp_id, size_t comp_cap)
{
    if (!src || !out || len < sizeof(lark_welcome_t)) return false;
    memcpy(out, src, sizeof(*out));
    size_t need = sizeof(*out) + out->id_len + out->comp_len;
    if (len < need) return false;
    size_t off = sizeof(*out);
    if (term_id && term_cap) {
        size_t n = out->id_len < term_cap - 1 ? out->id_len : term_cap - 1;
        if (n) memcpy(term_id, src + off, n);
        term_id[n] = '\0';
    }
    off += out->id_len;
    if (comp_id && comp_cap) {
        size_t n = out->comp_len < comp_cap - 1 ? out->comp_len : comp_cap - 1;
        if (n) memcpy(comp_id, src + off, n);
        comp_id[n] = '\0';
    }
    return true;
}

static inline bool lark_decode_reject(const uint8_t *src, size_t len, uint8_t *reason,
                                      char *msg, size_t msg_cap)
{
    if (!src || len < sizeof(lark_reject_t)) return false;
    lark_reject_t r;
    memcpy(&r, src, sizeof(r));
    if (len < sizeof(r) + r.msg_len) return false;
    if (reason) *reason = r.reason;
    if (msg && msg_cap) {
        size_t n = r.msg_len < msg_cap - 1 ? r.msg_len : msg_cap - 1;
        if (n) memcpy(msg, src + sizeof(r), n);
        msg[n] = '\0';
    }
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

static inline bool lark_parse_bridge_url(const char *url, char *host_out, size_t host_cap, uint16_t *port_out)
{
    if (!url || !host_out || !host_cap || !port_out) return false;
    const char *p = NULL;
    bool custom_port = false;
    if (!strncmp(url, "http://", 7)) {
        p = url + 7;
    } else if (!strncmp(url, "https://", 8)) {
        p = url + 8;
    } else if (!strncmp(url, "ws://", 5)) {
        p = url + 5;
    } else if (!strncmp(url, "wss://", 6)) {
        p = url + 6;
    } else if (!strncmp(url, "lark://", 7)) {
        p = url + 7;
        custom_port = true;
    } else {
        return false;
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
