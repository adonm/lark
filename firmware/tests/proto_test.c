#include "proto.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static void test_tcp_header(void)
{
    uint8_t buf[LARK_PROTO_HDR_SIZE];
    lark_pack_hdr(buf, LARK_MSG_FRAME, 0x1234, 4096);
    assert(buf[0] == LARK_PROTO_MAGIC);
    assert(buf[1] == LARK_MSG_FRAME);
    assert(buf[2] == 0x34 && buf[3] == 0x12);
    assert(buf[4] == 0x00 && buf[5] == 0x10 && buf[6] == 0x00 && buf[7] == 0x00);

    lark_hdr_t hdr = { 0 };
    assert(lark_unpack_hdr(buf, &hdr));
    assert(hdr.magic == LARK_PROTO_MAGIC);
    assert(hdr.type == LARK_MSG_FRAME);
    assert(hdr.seq == 0x1234);
    assert(hdr.length == 4096);

    // Reject bad magic
    buf[0] = 0x4D;
    assert(!lark_unpack_hdr(buf, &hdr));

    // Reject oversized payload (> 320 KB)
    lark_pack_hdr(buf, LARK_MSG_FRAME, 1, LARK_PROTO_MAX_PAYLOAD + 1);
    assert(!lark_unpack_hdr(buf, &hdr));

    // Accept exact max payload
    lark_pack_hdr(buf, LARK_MSG_FRAME, 1, LARK_PROTO_MAX_PAYLOAD);
    assert(lark_unpack_hdr(buf, &hdr));
    assert(hdr.length == LARK_PROTO_MAX_PAYLOAD);
}

static void test_rtp_header(void)
{
    uint8_t pkt[LARK_RTP_PACKET_BYTES];
    lark_rtp_encode(pkt, LARK_RTP_PT_REC_PCM, 0xABCD, 0x11223344u, 0xDEADBEEFu);
    assert(pkt[0] == 0x80);
    assert(pkt[1] == LARK_RTP_PT_REC_PCM);
    assert(pkt[2] == 0xAB && pkt[3] == 0xCD);
    assert(pkt[4] == 0x11 && pkt[5] == 0x22 && pkt[6] == 0x33 && pkt[7] == 0x44);
    assert(pkt[8] == 0xDE && pkt[9] == 0xAD && pkt[10] == 0xBE && pkt[11] == 0xEF);

    uint8_t pt = 0;
    uint16_t seq = 0;
    uint32_t ts = 0, ssrc = 0;
    assert(lark_rtp_decode(pkt, sizeof(pkt), &pt, &seq, &ts, &ssrc));
    assert(pt == LARK_RTP_PT_REC_PCM);
    assert(seq == 0xABCD);
    assert(ts == 0x11223344u);
    assert(ssrc == 0xDEADBEEFu);

    assert(!lark_rtp_decode(pkt, LARK_RTP_HDR_SIZE - 1, &pt, &seq, &ts, &ssrc));
    pkt[0] = 0x00;
    assert(!lark_rtp_decode(pkt, sizeof(pkt), &pt, &seq, &ts, &ssrc));
}

static void test_url_parsing(void)
{
    char host[128];
    uint16_t port = 0;

    assert(lark_parse_bridge_url("http://192.168.1.42:8765", host, sizeof(host), &port));
    assert(strcmp(host, "192.168.1.42") == 0 && port == 8766);

    assert(lark_parse_bridge_url("https://bridge.local:8443/", host, sizeof(host), &port));
    assert(strcmp(host, "bridge.local") == 0 && port == 8766);

    assert(lark_parse_bridge_url("ws://10.0.0.5:8765/ws", host, sizeof(host), &port));
    assert(strcmp(host, "10.0.0.5") == 0 && port == 8766);

    assert(lark_parse_bridge_url("lark://10.0.0.5:9001", host, sizeof(host), &port));
    assert(strcmp(host, "10.0.0.5") == 0 && port == 9001);

    assert(lark_parse_bridge_url("lark://lark.local", host, sizeof(host), &port));
    assert(strcmp(host, "lark.local") == 0 && port == 8766);

    assert(lark_parse_bridge_url("lark://[::1]:7777", host, sizeof(host), &port));
    assert(strcmp(host, "::1") == 0 && port == 7777);

    assert(!lark_parse_bridge_url("ftp://10.0.0.5", host, sizeof(host), &port));
    assert(!lark_parse_bridge_url("http://user@10.0.0.5:8765", host, sizeof(host), &port));
    assert(!lark_parse_bridge_url("lark://10.0.0.5:99999", host, sizeof(host), &port));
}

static void test_hello_welcome_reject(void)
{
    lark_hello_t h = {
        .proto_version = LARK_PROTO_VERSION,
        .kind = LARK_KIND_HARDWARE,
        .caps_mask = LARK_CAP_BATTERY | LARK_CAP_DRAG,
        .display_formats = LARK_DISP_RGB565,
        .frame_codecs = LARK_CODEC_RAW | LARK_CODEC_RLE565 | LARK_CODEC_LZ4 | LARK_CODEC_JPEG,
        .width = 360,
        .height = 360,
        .shape = LARK_SHAPE_ROUND,
        .flags = 0,
        .udp_port = 40001,
        .id_len = 10,
        .comp_len = 0,
        .board_len = 9,
        .fw_len = 6,
        .tok_len = 5,
    };
    uint8_t buf[128];
    size_t n = lark_encode_hello(buf, sizeof(buf), &h, "vocat-33d8", "", "esp-vocat", "0.13.0", "token");
    assert(n == sizeof(lark_hello_t) + 10 + 0 + 9 + 6 + 5);
    assert(buf[0] == LARK_PROTO_VERSION);
    assert(buf[1] == LARK_KIND_HARDWARE);
    assert(memcmp(buf + sizeof(lark_hello_t), "vocat-33d8esp-vocat0.13.0token", 30) == 0);
    assert(lark_encode_hello(buf, 10, &h, "vocat-33d8", "", "esp-vocat", "0.13.0", "token") == 0);

    uint8_t wbuf[sizeof(lark_welcome_t) + 14];
    lark_welcome_t w = {
        .proto_version = LARK_PROTO_VERSION,
        .flags = LARK_WELCOME_FLAG_BRIDGE_EP,
        .udp_port = 8766,
        .ssrc = 0x11223344u,
        .max_msg_bytes = LARK_PROTO_MAX_PAYLOAD,
        .resume_window_ms = LARK_RESUME_WINDOW_MS,
        .seq = 42,
        .id_len = 10,
        .comp_len = 4,
    };
    for (int i = 0; i < LARK_RESUME_TOKEN_LEN; ++i) w.resume_token[i] = (uint8_t)(i + 1);
    memcpy(wbuf, &w, sizeof(w));
    memcpy(wbuf + sizeof(w), "vocat-33d8cake", 14);

    lark_welcome_t wout = {0};
    char tid[32], cid[32];
    assert(lark_decode_welcome(wbuf, sizeof(wbuf), &wout, tid, sizeof(tid), cid, sizeof(cid)));
    assert(wout.proto_version == LARK_PROTO_VERSION);
    assert(wout.flags == LARK_WELCOME_FLAG_BRIDGE_EP);
    assert(wout.udp_port == 8766);
    assert(wout.ssrc == 0x11223344u);
    assert(wout.seq == 42);
    assert(wout.resume_token[0] == 1 && wout.resume_token[15] == 16);
    assert(strcmp(tid, "vocat-33d8") == 0);
    assert(strcmp(cid, "cake") == 0);
    assert(!lark_decode_welcome(wbuf, sizeof(wbuf) - 1, &wout, tid, sizeof(tid), cid, sizeof(cid)));

    uint8_t rbuf[sizeof(lark_reject_t) + 11];
    lark_reject_t r = { .reason = LARK_REJECT_BAD_VERSION, .msg_len = 11 };
    memcpy(rbuf, &r, sizeof(r));
    memcpy(rbuf + sizeof(r), "bad version", 11);
    uint8_t reason = 0;
    char msg[32];
    assert(lark_decode_reject(rbuf, sizeof(rbuf), &reason, msg, sizeof(msg)));
    assert(reason == LARK_REJECT_BAD_VERSION);
    assert(strcmp(msg, "bad version") == 0);
    assert(!lark_decode_reject(rbuf, sizeof(rbuf) - 1, &reason, msg, sizeof(msg)));
}

int main(void)
{
    test_tcp_header();
    test_rtp_header();
    test_url_parsing();
    test_hello_welcome_reject();
    printf("PASS host proto tests\n");
    return 0;
}
