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
    lark_rtp_encode(pkt, LARK_RTP_PT_CALL_UP, 0xABCD, 0x11223344u, 0xDEADBEEFu);
    assert(pkt[0] == 0x80);
    assert(pkt[1] == LARK_RTP_PT_CALL_UP);
    assert(pkt[2] == 0xAB && pkt[3] == 0xCD);
    assert(pkt[4] == 0x11 && pkt[5] == 0x22 && pkt[6] == 0x33 && pkt[7] == 0x44);
    assert(pkt[8] == 0xDE && pkt[9] == 0xAD && pkt[10] == 0xBE && pkt[11] == 0xEF);

    uint8_t pt = 0;
    uint16_t seq = 0;
    uint32_t ts = 0, ssrc = 0;
    assert(lark_rtp_decode(pkt, sizeof(pkt), &pt, &seq, &ts, &ssrc));
    assert(pt == LARK_RTP_PT_CALL_UP);
    assert(seq == 0xABCD);
    assert(ts == 0x11223344u);
    assert(ssrc == 0xDEADBEEFu);

    assert(!lark_rtp_decode(pkt, LARK_RTP_HDR_SIZE - 1, &pt, &seq, &ts, &ssrc));
    pkt[0] = 0x00;
    assert(!lark_rtp_decode(pkt, sizeof(pkt), &pt, &seq, &ts, &ssrc));
}

static void test_jitter_buffer(void)
{
    lark_jitter_t jb;
    lark_jitter_reset(&jb);
    int16_t p1[LARK_RTP_SAMPLES], p2[LARK_RTP_SAMPLES], p3[LARK_RTP_SAMPLES], out[LARK_RTP_SAMPLES];
    for (int i = 0; i < LARK_RTP_SAMPLES; ++i) {
        p1[i] = (int16_t)(100 + i);
        p2[i] = (int16_t)(200 + i);
        p3[i] = (int16_t)(300 + i);
    }

    // Out-of-order arrival across wraparound: 65535, 0, 1
    assert(lark_jitter_push(&jb, 65535, p1, sizeof(p1)));
    assert(lark_jitter_push(&jb, 1, p3, sizeof(p3)));
    assert(lark_jitter_push(&jb, 0, p2, sizeof(p2)));

    // Stale packet should be rejected
    assert(!lark_jitter_push(&jb, 65534, p1, sizeof(p1)));

    assert(lark_jitter_pop(&jb, out) == sizeof(out));
    assert(out[0] == 100 && out[10] == 110);
    assert(lark_jitter_pop(&jb, out) == sizeof(out));
    assert(out[0] == 200 && out[10] == 210);
    assert(lark_jitter_pop(&jb, out) == sizeof(out));
    assert(out[0] == 300 && out[10] == 310);
    assert(lark_jitter_pop(&jb, out) == 0);

    // Packet loss: 2 is lost, 3 and 4 arrive -> pops 3 and 4
    assert(lark_jitter_push(&jb, 3, p2, sizeof(p2)));
    assert(lark_jitter_push(&jb, 4, p3, sizeof(p3)));
    assert(lark_jitter_pop(&jb, out) == sizeof(out));
    assert(out[0] == 200);
    assert(lark_jitter_pop(&jb, out) == sizeof(out));
    assert(out[0] == 300);
    assert(lark_jitter_pop(&jb, out) == 0);
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

int main(void)
{
    test_tcp_header();
    test_rtp_header();
    test_jitter_buffer();
    test_url_parsing();
    printf("PASS host proto tests\n");
    return 0;
}
