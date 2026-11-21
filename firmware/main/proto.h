#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

enum {
    LARK_PROTO_MAGIC = 0x4C, // 'L'
    LARK_PROTO_HDR_SIZE = 8,
    LARK_PROTO_MAX_PAYLOAD = 320 * 1024,
    LARK_DEFAULT_DEVICE_PORT = 8766,
    LARK_DEFAULT_HTTP_PORT = 8765,
};

// Device -> Bridge TCP message types
enum {
    LARK_MSG_HELLO = 0x01,
    LARK_MSG_STATUS = 0x02,
    LARK_MSG_POINTER = 0x03,
    LARK_MSG_WAKE_WORD = 0x04,
    LARK_MSG_REC_START = 0x05,
    LARK_MSG_REC_END = 0x06,
    LARK_MSG_TTS_ACK = 0x07,
    LARK_MSG_CALL_FAILED = 0x08,
    LARK_MSG_PING = 0x09,
    LARK_MSG_PONG = 0x0A,
};

// Bridge -> Device TCP message types
enum {
    LARK_MSG_HELLO_OK = 0x81,
    LARK_MSG_FRAME = 0x82,
    LARK_MSG_WAKE_WORD_CTRL = 0x83,
    LARK_MSG_REC_CTRL = 0x84,
    LARK_MSG_TTS_CTRL = 0x85,
    LARK_MSG_TTS_PCM = 0x86,
    LARK_MSG_CALL_CTRL = 0x87,
    LARK_MSG_CHIME = 0x88,
    LARK_MSG_POWER = 0x89,
    LARK_MSG_VOLUME = 0x8A,
};

// Capability bits in MSG_HELLO.caps_mask
enum {
    LARK_CAP_BATTERY = 1u << 0,
    LARK_CAP_HEAD_PADS = 1u << 1,
    LARK_CAP_CALL = 1u << 2,
    LARK_CAP_CALL_PCM = 1u << 3,
    LARK_CAP_DRAG = 1u << 4,
    LARK_CAP_POINTER_REV = 1u << 5,
    LARK_CAP_BRIDGE_ENDPOINT = 1u << 6,
    LARK_CAP_PANEL_SLEEP = 1u << 7,
    LARK_CAP_WAKE_WORD = 1u << 8,
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

// Call ctrl ops
enum {
    LARK_CALL_END = 0,
    LARK_CALL_START = 1,
    LARK_CALL_MUTE = 2,
    LARK_CALL_UNMUTE = 3,
};

// Chime kinds
enum {
    LARK_CHIME_TAP = 0,
    LARK_CHIME_THINK = 1,
    LARK_CHIME_CANCEL = 2,
    LARK_CHIME_INTERCOM = 3,
    LARK_CHIME_RING = 4,
};

// Hello OK flags
enum {
    LARK_HELLO_FLAG_BRIDGE_EP = 1u << 0,
};

// Status flags
enum {
    LARK_STATUS_SLEEPING = 1u << 0,
};

// RTP (RFC 3550) constants for 20 ms 16 kHz mono 16-bit PCM
enum {
    LARK_RTP_VERSION_BYTE = 0x80,
    LARK_RTP_HDR_SIZE = 12,
    LARK_RTP_SAMPLES = 320,
    LARK_RTP_PCM_BYTES = LARK_RTP_SAMPLES * 2,
    LARK_RTP_PACKET_BYTES = LARK_RTP_HDR_SIZE + LARK_RTP_PCM_BYTES,
    LARK_RTP_PT_REC_PCM = 96,
    LARK_RTP_PT_CALL_UP = 97,
    LARK_RTP_PT_CALL_DOWN = 98,
    LARK_JITTER_SLOTS = 3,
};

typedef struct __attribute__((packed)) {
    uint8_t magic;
    uint8_t type;
    uint16_t seq;
    uint32_t length;
} lark_hdr_t;

typedef struct __attribute__((packed)) {
    uint32_t caps_mask;
    uint16_t width;
    uint16_t height;
    uint16_t udp_port;
    uint8_t id_len;
    uint8_t board_len;
    uint8_t fw_len;
    uint8_t tok_len;
} lark_hello_t;

typedef struct __attribute__((packed)) {
    uint16_t udp_port;
    uint16_t flags;
    uint32_t ssrc;
    uint32_t session_id;
} lark_hello_ok_t;

typedef struct __attribute__((packed)) {
    int16_t wifi_rssi;
    int16_t battery_pct;
    uint8_t charging;
    uint8_t cpu_pct;
    uint8_t flags;
    uint8_t pad;
    uint32_t free_heap;
    uint32_t uptime_s;
} lark_status_t;

typedef struct __attribute__((packed)) {
    uint8_t kind;
    uint8_t pad;
    uint16_t x;
    uint16_t y;
    uint16_t rev_lo;
} lark_pointer_t;

typedef struct __attribute__((packed)) {
    uint16_t session;
    uint8_t target;
    uint8_t pad;
} lark_rec_start_t;

typedef struct __attribute__((packed)) {
    uint16_t session;
    uint8_t target;
    uint8_t cancelled;
} lark_rec_end_t;

typedef struct __attribute__((packed)) {
    uint8_t op;
    uint8_t target;
    uint16_t session;
} lark_rec_ctrl_t;

_Static_assert(sizeof(lark_hdr_t) == 8, "lark_hdr_t must be 8 bytes");
_Static_assert(sizeof(lark_hello_t) == 14, "lark_hello_t must be 14 bytes");
_Static_assert(sizeof(lark_hello_ok_t) == 12, "lark_hello_ok_t must be 12 bytes");
_Static_assert(sizeof(lark_status_t) == 16, "lark_status_t must be 16 bytes");
_Static_assert(sizeof(lark_pointer_t) == 8, "lark_pointer_t must be 8 bytes");
_Static_assert(sizeof(lark_rec_start_t) == 4, "lark_rec_start_t must be 4 bytes");
_Static_assert(sizeof(lark_rec_end_t) == 4, "lark_rec_end_t must be 4 bytes");
_Static_assert(sizeof(lark_rec_ctrl_t) == 4, "lark_rec_ctrl_t must be 4 bytes");

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

typedef struct {
    bool valid;
    uint16_t seq;
    uint16_t bytes;
    int16_t pcm[LARK_RTP_SAMPLES];
} lark_jitter_slot_t;

typedef struct {
    bool synced;
    uint16_t next_seq;
    lark_jitter_slot_t slots[LARK_JITTER_SLOTS];
} lark_jitter_t;

static inline void lark_jitter_reset(lark_jitter_t *jb)
{
    memset(jb, 0, sizeof(*jb));
}

static inline int16_t lark_seq_diff(uint16_t a, uint16_t b)
{
    return (int16_t)(a - b);
}

static inline bool lark_jitter_push(lark_jitter_t *jb, uint16_t seq, const void *pcm, size_t bytes)
{
    if (!jb || !pcm || !bytes || bytes > LARK_RTP_PCM_BYTES || (bytes % 2) != 0) return false;
    if (!jb->synced) {
        jb->synced = true;
        jb->next_seq = seq;
    }
    int16_t diff = lark_seq_diff(seq, jb->next_seq);
    if (diff < 0) return false; // Stale or duplicate packet.
    if (diff > 100) {
        lark_jitter_reset(jb);
        jb->synced = true;
        jb->next_seq = seq;
    } else if (diff >= LARK_JITTER_SLOTS) {
        uint16_t new_next = (uint16_t)(seq - (LARK_JITTER_SLOTS - 1));
        for (int i = 0; i < LARK_JITTER_SLOTS; ++i) {
            if (jb->slots[i].valid && lark_seq_diff(jb->slots[i].seq, new_next) < 0) {
                jb->slots[i].valid = false;
            }
        }
        jb->next_seq = new_next;
    }
    int free_idx = -1;
    for (int i = 0; i < LARK_JITTER_SLOTS; ++i) {
        if (jb->slots[i].valid && jb->slots[i].seq == seq) {
            free_idx = i;
            break;
        }
        if (!jb->slots[i].valid && free_idx < 0) free_idx = i;
    }
    if (free_idx < 0) {
        // All 3 slots occupied: evict the oldest sequence.
        free_idx = 0;
        for (int i = 1; i < LARK_JITTER_SLOTS; ++i) {
            if (lark_seq_diff(jb->slots[i].seq, jb->slots[free_idx].seq) < 0) free_idx = i;
        }
    }
    lark_jitter_slot_t *slot = &jb->slots[free_idx];
    slot->valid = true;
    slot->seq = seq;
    slot->bytes = (uint16_t)bytes;
    memcpy(slot->pcm, pcm, bytes);
    return true;
}

static inline size_t lark_jitter_pop(lark_jitter_t *jb, int16_t *out_pcm)
{
    if (!jb || !jb->synced || !out_pcm) return 0;
    for (int step = 0; step < LARK_JITTER_SLOTS; ++step) {
        for (int i = 0; i < LARK_JITTER_SLOTS; ++i) {
            lark_jitter_slot_t *slot = &jb->slots[i];
            if (slot->valid && slot->seq == jb->next_seq) {
                size_t bytes = slot->bytes;
                memcpy(out_pcm, slot->pcm, bytes);
                slot->valid = false;
                jb->next_seq++;
                return bytes;
            }
        }
        // Only skip a missing slot if a newer packet is already buffered.
        bool newer = false;
        for (int i = 0; i < LARK_JITTER_SLOTS; ++i) {
            if (jb->slots[i].valid && lark_seq_diff(jb->slots[i].seq, jb->next_seq) > 0) {
                newer = true;
                break;
            }
        }
        if (!newer) break;
        jb->next_seq++;
    }
    return 0;
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
