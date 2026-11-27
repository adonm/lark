#include "proto.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

#include "testdata/proto_fixtures.h"

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

    assert(lark_parse_edge_url("http://192.168.1.42:8765", host, sizeof(host), &port));
    assert(strcmp(host, "192.168.1.42") == 0 && port == 8766);

    assert(lark_parse_edge_url("10.0.0.5", host, sizeof(host), &port));
    assert(strcmp(host, "10.0.0.5") == 0 && port == 8766);

    assert(lark_parse_edge_url("edge.local:9001", host, sizeof(host), &port));
    assert(strcmp(host, "edge.local") == 0 && port == 9001);

    assert(lark_parse_edge_url("lark://10.0.0.5:9001", host, sizeof(host), &port));
    assert(strcmp(host, "10.0.0.5") == 0 && port == 9001);

    assert(lark_parse_edge_url("lark://lark.local", host, sizeof(host), &port));
    assert(strcmp(host, "lark.local") == 0 && port == 8766);

    assert(lark_parse_edge_url("lark://[::1]:7777", host, sizeof(host), &port));
    assert(strcmp(host, "::1") == 0 && port == 7777);

    assert(!lark_parse_edge_url("https://edge.local:8443/", host, sizeof(host), &port));
    assert(!lark_parse_edge_url("ws://10.0.0.5:8765/ws", host, sizeof(host), &port));
    assert(!lark_parse_edge_url("wss://10.0.0.5:8765/ws", host, sizeof(host), &port));
    assert(!lark_parse_edge_url("ftp://10.0.0.5", host, sizeof(host), &port));
    assert(!lark_parse_edge_url("http://user@10.0.0.5:8765", host, sizeof(host), &port));
    assert(!lark_parse_edge_url("lark://10.0.0.5:99999", host, sizeof(host), &port));
}

// Each fixture below was produced by the Go codec from lark/protospec/spec.go.
// Encoding the same sample in C must match byte for byte, and decoding the
// fixture must yield the sample fields.
static void test_hello_fixture(void)
{
    lark_hello_t h = {
        .proto_version = FIXTURE_HELLO_PROTO_VERSION,
        .kind = FIXTURE_HELLO_KIND,
        .caps_mask = FIXTURE_HELLO_CAPS_MASK,
        .display_formats = FIXTURE_HELLO_DISPLAY_FORMATS,
        .frame_codecs = FIXTURE_HELLO_FRAME_CODECS,
        .width = FIXTURE_HELLO_WIDTH,
        .height = FIXTURE_HELLO_HEIGHT,
        .shape = FIXTURE_HELLO_SHAPE,
        .flags = FIXTURE_HELLO_FLAGS,
        .udp_port = FIXTURE_HELLO_UDP_PORT,
        .id_len = strlen(FIXTURE_HELLO_ID_LEN),
        .comp_len = strlen(FIXTURE_HELLO_COMP_LEN),
        .board_len = strlen(FIXTURE_HELLO_BOARD_LEN),
        .fw_len = strlen(FIXTURE_HELLO_FW_LEN),
        .tok_len = strlen(FIXTURE_HELLO_TOK_LEN),
    };
    uint8_t buf[128];
    size_t n = lark_encode_hello(buf, sizeof(buf), &h, FIXTURE_HELLO_ID_LEN, FIXTURE_HELLO_COMP_LEN,
                                 FIXTURE_HELLO_BOARD_LEN, FIXTURE_HELLO_FW_LEN, FIXTURE_HELLO_TOK_LEN);
    assert(n == PROTO_FIXTURE_HELLO_LEN && memcmp(buf, proto_fixture_hello, n) == 0);
    assert(lark_encode_hello(buf, 10, &h, FIXTURE_HELLO_ID_LEN, FIXTURE_HELLO_COMP_LEN,
                             FIXTURE_HELLO_BOARD_LEN, FIXTURE_HELLO_FW_LEN, FIXTURE_HELLO_TOK_LEN) == 0);

    lark_hello_t out;
    char tid[32], cid[32], board[32], fw[32], tok[32];
    assert(lark_decode_hello(proto_fixture_hello, sizeof(proto_fixture_hello), &out, tid, sizeof(tid),
                             cid, sizeof(cid), board, sizeof(board), fw, sizeof(fw), tok, sizeof(tok)));
    assert(out.caps_mask == FIXTURE_HELLO_CAPS_MASK && out.udp_port == FIXTURE_HELLO_UDP_PORT);
    assert(strcmp(tid, FIXTURE_HELLO_ID_LEN) == 0 && strcmp(cid, FIXTURE_HELLO_COMP_LEN) == 0);
    assert(strcmp(board, FIXTURE_HELLO_BOARD_LEN) == 0 && strcmp(fw, FIXTURE_HELLO_FW_LEN) == 0);
    assert(strcmp(tok, FIXTURE_HELLO_TOK_LEN) == 0);
    assert(!lark_decode_hello(proto_fixture_hello, sizeof(proto_fixture_hello) - 1, &out, tid, sizeof(tid),
                              cid, sizeof(cid), board, sizeof(board), fw, sizeof(fw), tok, sizeof(tok)));
}

static void test_resume_fixture(void)
{
    lark_resume_t r = {
        .proto_version = FIXTURE_RESUME_PROTO_VERSION,
        .id_len = strlen(FIXTURE_RESUME_ID_LEN),
    };
    for (int i = 0; i < LARK_RESUME_TOKEN_LEN; ++i) r.resume_token[i] = (uint8_t)(i + 1);
    uint8_t buf[64];
    size_t n = lark_encode_resume(buf, sizeof(buf), &r, FIXTURE_RESUME_ID_LEN);
    assert(n == PROTO_FIXTURE_RESUME_LEN && memcmp(buf, proto_fixture_resume, n) == 0);

    lark_resume_t out;
    char id[32];
    assert(lark_decode_resume(proto_fixture_resume, sizeof(proto_fixture_resume), &out, id, sizeof(id)));
    assert(out.resume_token[0] == 1 && out.resume_token[15] == 16);
    assert(strcmp(id, FIXTURE_RESUME_ID_LEN) == 0);
}

static void test_telemetry_fixture(void)
{
    lark_telemetry_t t = {
        .wifi_rssi = FIXTURE_TELEMETRY_WIFI_RSSI,
        .battery_pct = FIXTURE_TELEMETRY_BATTERY_PCT,
        .charging = FIXTURE_TELEMETRY_CHARGING,
        .cpu_pct = FIXTURE_TELEMETRY_CPU_PCT,
        .flags = FIXTURE_TELEMETRY_FLAGS,
        .pad = FIXTURE_TELEMETRY_PAD,
        .free_heap = FIXTURE_TELEMETRY_FREE_HEAP,
        .uptime_s = FIXTURE_TELEMETRY_UPTIME_S,
    };
    uint8_t buf[32];
    size_t n = lark_encode_telemetry(buf, sizeof(buf), &t);
    assert(n == PROTO_FIXTURE_TELEMETRY_LEN && memcmp(buf, proto_fixture_telemetry, n) == 0);

    lark_telemetry_t out;
    assert(lark_decode_telemetry(proto_fixture_telemetry, sizeof(proto_fixture_telemetry), &out));
    assert(out.wifi_rssi == FIXTURE_TELEMETRY_WIFI_RSSI && out.free_heap == FIXTURE_TELEMETRY_FREE_HEAP);
}

static void test_pointer_fixture(void)
{
    lark_pointer_t p = {
        .kind = FIXTURE_POINTER_KIND,
        .pad = FIXTURE_POINTER_PAD,
        .x = FIXTURE_POINTER_X,
        .y = FIXTURE_POINTER_Y,
        .rev_lo = FIXTURE_POINTER_REV_LO,
    };
    uint8_t buf[16];
    size_t n = lark_encode_pointer(buf, sizeof(buf), &p);
    assert(n == PROTO_FIXTURE_POINTER_LEN && memcmp(buf, proto_fixture_pointer, n) == 0);

    lark_pointer_t out;
    assert(lark_decode_pointer(proto_fixture_pointer, sizeof(proto_fixture_pointer), &out));
    assert(out.kind == FIXTURE_POINTER_KIND && out.x == FIXTURE_POINTER_X && out.y == FIXTURE_POINTER_Y);
}

static void test_rec_fixtures(void)
{
    lark_rec_start_t rs = { .session = FIXTURE_REC_START_SESSION, .target_len = strlen(FIXTURE_REC_START_TARGET_LEN) };
    uint8_t buf[32];
    size_t n = lark_encode_rec_start(buf, sizeof(buf), &rs, FIXTURE_REC_START_TARGET_LEN);
    assert(n == PROTO_FIXTURE_REC_START_LEN && memcmp(buf, proto_fixture_rec_start, n) == 0);
    lark_rec_start_t rsout;
    char target[32];
    assert(lark_decode_rec_start(proto_fixture_rec_start, sizeof(proto_fixture_rec_start), &rsout, target, sizeof(target)));
    assert(rsout.session == FIXTURE_REC_START_SESSION && strcmp(target, FIXTURE_REC_START_TARGET_LEN) == 0);

    lark_rec_end_t re = {
        .session = FIXTURE_REC_END_SESSION,
        .cancelled = FIXTURE_REC_END_CANCELLED,
        .target_len = strlen(FIXTURE_REC_END_TARGET_LEN),
    };
    n = lark_encode_rec_end(buf, sizeof(buf), &re, FIXTURE_REC_END_TARGET_LEN);
    assert(n == PROTO_FIXTURE_REC_END_LEN && memcmp(buf, proto_fixture_rec_end, n) == 0);
    lark_rec_end_t reout;
    assert(lark_decode_rec_end(proto_fixture_rec_end, sizeof(proto_fixture_rec_end), &reout, target, sizeof(target)));
    assert(reout.cancelled == FIXTURE_REC_END_CANCELLED && strcmp(target, FIXTURE_REC_END_TARGET_LEN) == 0);
}

static void test_tts_fixtures(void)
{
    lark_tts_ack_t ack = { .aborted = FIXTURE_TTS_ACK_ABORTED, .audio_id = FIXTURE_TTS_ACK_AUDIO_ID };
    uint8_t buf[16];
    size_t n = lark_encode_tts_ack(buf, sizeof(buf), &ack);
    assert(n == PROTO_FIXTURE_TTS_ACK_LEN && memcmp(buf, proto_fixture_tts_ack, n) == 0);
    lark_tts_ack_t aout;
    assert(lark_decode_tts_ack(proto_fixture_tts_ack, sizeof(proto_fixture_tts_ack), &aout));
    assert(aout.aborted == FIXTURE_TTS_ACK_ABORTED && aout.audio_id == FIXTURE_TTS_ACK_AUDIO_ID);

    lark_tts_ctrl_t ctrl = { .op = FIXTURE_TTS_CTRL_OP, .audio_id = FIXTURE_TTS_CTRL_AUDIO_ID };
    n = lark_encode_tts_ctrl(buf, sizeof(buf), &ctrl);
    assert(n == PROTO_FIXTURE_TTS_CTRL_LEN && memcmp(buf, proto_fixture_tts_ctrl, n) == 0);
    lark_tts_ctrl_t cout;
    assert(lark_decode_tts_ctrl(proto_fixture_tts_ctrl, sizeof(proto_fixture_tts_ctrl), &cout));
    assert(cout.op == FIXTURE_TTS_CTRL_OP && cout.audio_id == FIXTURE_TTS_CTRL_AUDIO_ID);
}

static void test_ping_pong_fixtures(void)
{
    lark_ping_t ping = { .payload = { 0xAA, 0xBB, 0xCC, 0xDD } };
    uint8_t buf[8];
    size_t n = lark_encode_ping(buf, sizeof(buf), &ping);
    assert(n == PROTO_FIXTURE_PING_LEN && memcmp(buf, proto_fixture_ping, n) == 0);
    lark_ping_t pout;
    assert(lark_decode_ping(proto_fixture_ping, sizeof(proto_fixture_ping), &pout));
    assert(memcmp(pout.payload, ping.payload, sizeof(ping.payload)) == 0);

    lark_pong_t pong = { .payload = { 0xAA, 0xBB, 0xCC, 0xDD } };
    n = lark_encode_pong(buf, sizeof(buf), &pong);
    assert(n == PROTO_FIXTURE_PONG_LEN && memcmp(buf, proto_fixture_pong, n) == 0);
}

static void test_welcome_fixture(void)
{
    lark_welcome_t w = {
        .proto_version = FIXTURE_WELCOME_PROTO_VERSION,
        .flags = FIXTURE_WELCOME_FLAGS,
        .udp_port = FIXTURE_WELCOME_UDP_PORT,
        .ssrc = FIXTURE_WELCOME_SSRC,
        .max_msg_bytes = FIXTURE_WELCOME_MAX_MSG_BYTES,
        .resume_window_ms = FIXTURE_WELCOME_RESUME_WINDOW_MS,
        .id_len = strlen(FIXTURE_WELCOME_ID_LEN),
        .comp_len = strlen(FIXTURE_WELCOME_COMP_LEN),
    };
    for (int i = 0; i < LARK_RESUME_TOKEN_LEN; ++i) w.resume_token[i] = (uint8_t)(i + 1);
    uint8_t buf[128];
    size_t n = lark_encode_welcome(buf, sizeof(buf), &w, FIXTURE_WELCOME_ID_LEN, FIXTURE_WELCOME_COMP_LEN);
    assert(n == PROTO_FIXTURE_WELCOME_LEN && memcmp(buf, proto_fixture_welcome, n) == 0);

    lark_welcome_t out;
    char tid[32], cid[32];
    assert(lark_decode_welcome(proto_fixture_welcome, sizeof(proto_fixture_welcome), &out,
                               tid, sizeof(tid), cid, sizeof(cid)));
    assert(out.udp_port == FIXTURE_WELCOME_UDP_PORT && out.ssrc == FIXTURE_WELCOME_SSRC);
    assert(out.resume_token[0] == 1 && out.resume_token[15] == 16);
    assert(strcmp(tid, FIXTURE_WELCOME_ID_LEN) == 0 && strcmp(cid, FIXTURE_WELCOME_COMP_LEN) == 0);
    assert(!lark_decode_welcome(proto_fixture_welcome, sizeof(proto_fixture_welcome) - 1, &out,
                                tid, sizeof(tid), cid, sizeof(cid)));
}

static void test_reject_fixture(void)
{
    lark_reject_t r = { .reason = FIXTURE_REJECT_REASON, .msg_len = strlen(FIXTURE_REJECT_MSG_LEN) };
    uint8_t buf[32];
    size_t n = lark_encode_reject(buf, sizeof(buf), &r, FIXTURE_REJECT_MSG_LEN);
    assert(n == PROTO_FIXTURE_REJECT_LEN && memcmp(buf, proto_fixture_reject, n) == 0);

    lark_reject_t out;
    char msg[32];
    assert(lark_decode_reject(proto_fixture_reject, sizeof(proto_fixture_reject), &out, msg, sizeof(msg)));
    assert(out.reason == FIXTURE_REJECT_REASON && strcmp(msg, FIXTURE_REJECT_MSG_LEN) == 0);
    assert(!lark_decode_reject(proto_fixture_reject, sizeof(proto_fixture_reject) - 1, &out, msg, sizeof(msg)));
}

static void test_rec_ctrl_fixture(void)
{
    lark_rec_ctrl_t c = {
        .op = FIXTURE_REC_CTRL_OP,
        .flags = FIXTURE_REC_CTRL_FLAGS,
        .session = FIXTURE_REC_CTRL_SESSION,
        .target_len = strlen(FIXTURE_REC_CTRL_TARGET_LEN),
    };
    uint8_t buf[32];
    size_t n = lark_encode_rec_ctrl(buf, sizeof(buf), &c, FIXTURE_REC_CTRL_TARGET_LEN);
    assert(n == PROTO_FIXTURE_REC_CTRL_LEN && memcmp(buf, proto_fixture_rec_ctrl, n) == 0);

    lark_rec_ctrl_t out;
    char target[32];
    assert(lark_decode_rec_ctrl(proto_fixture_rec_ctrl, sizeof(proto_fixture_rec_ctrl), &out, target, sizeof(target)));
    assert(out.op == FIXTURE_REC_CTRL_OP && out.flags == FIXTURE_REC_CTRL_FLAGS);
    assert(out.session == FIXTURE_REC_CTRL_SESSION && strcmp(target, FIXTURE_REC_CTRL_TARGET_LEN) == 0);
}

static void test_simple_fixtures(void)
{
    uint8_t buf[32];
    lark_chime_t chime = { .kind = FIXTURE_CHIME_KIND };
    size_t n = lark_encode_chime(buf, sizeof(buf), &chime);
    assert(n == PROTO_FIXTURE_CHIME_LEN && memcmp(buf, proto_fixture_chime, n) == 0);
    lark_chime_t cout;
    assert(lark_decode_chime(proto_fixture_chime, sizeof(proto_fixture_chime), &cout));
    assert(cout.kind == FIXTURE_CHIME_KIND);

    lark_power_t power = { .level = FIXTURE_POWER_LEVEL };
    n = lark_encode_power(buf, sizeof(buf), &power);
    assert(n == PROTO_FIXTURE_POWER_LEN && memcmp(buf, proto_fixture_power, n) == 0);
    lark_power_t pout;
    assert(lark_decode_power(proto_fixture_power, sizeof(proto_fixture_power), &pout));
    assert(pout.level == FIXTURE_POWER_LEVEL);

    lark_volume_t vol = {
        .level = FIXTURE_VOLUME_LEVEL,
        .persist = FIXTURE_VOLUME_PERSIST,
        .id_len = strlen(FIXTURE_VOLUME_ID_LEN),
        .comp_len = strlen(FIXTURE_VOLUME_COMP_LEN),
    };
    n = lark_encode_volume(buf, sizeof(buf), &vol, FIXTURE_VOLUME_ID_LEN, FIXTURE_VOLUME_COMP_LEN);
    assert(n == PROTO_FIXTURE_VOLUME_LEN && memcmp(buf, proto_fixture_volume, n) == 0);
    lark_volume_t vout;
    char tid[32], cid[32];
    assert(lark_decode_volume(proto_fixture_volume, sizeof(proto_fixture_volume), &vout, tid, sizeof(tid), cid, sizeof(cid)));
    assert(vout.level == FIXTURE_VOLUME_LEVEL && strcmp(tid, FIXTURE_VOLUME_ID_LEN) == 0);
    assert(strcmp(cid, FIXTURE_VOLUME_COMP_LEN) == 0);

    lark_status_t status = {
        .state = FIXTURE_STATUS_STATE,
    };
    n = lark_encode_status(buf, sizeof(buf), &status);
    assert(n == PROTO_FIXTURE_STATUS_LEN && memcmp(buf, proto_fixture_status, n) == 0);
    lark_status_t sout;
    assert(lark_decode_status(proto_fixture_status, sizeof(proto_fixture_status), &sout));
    assert(sout.state == FIXTURE_STATUS_STATE);

    lark_voice_owner_t owner = {
        .comp_len = strlen(FIXTURE_VOICE_OWNER_COMP_LEN),
        .term_len = strlen(FIXTURE_VOICE_OWNER_TERM_LEN),
    };
    n = lark_encode_voice_owner(buf, sizeof(buf), &owner, FIXTURE_VOICE_OWNER_COMP_LEN,
                                FIXTURE_VOICE_OWNER_TERM_LEN);
    assert(n == PROTO_FIXTURE_VOICE_OWNER_LEN && memcmp(buf, proto_fixture_voice_owner, n) == 0);
    lark_voice_owner_t oout;
    assert(lark_decode_voice_owner(proto_fixture_voice_owner, sizeof(proto_fixture_voice_owner),
                                   &oout, cid, sizeof(cid), tid, sizeof(tid)));
    assert(strcmp(cid, FIXTURE_VOICE_OWNER_COMP_LEN) == 0 && strcmp(tid, FIXTURE_VOICE_OWNER_TERM_LEN) == 0);
}

int main(void)
{
    test_tcp_header();
    test_rtp_header();
    test_url_parsing();
    test_hello_fixture();
    test_resume_fixture();
    test_telemetry_fixture();
    test_pointer_fixture();
    test_rec_fixtures();
    test_tts_fixtures();
    test_ping_pong_fixtures();
    test_welcome_fixture();
    test_reject_fixture();
    test_rec_ctrl_fixture();
    test_simple_fixtures();
    printf("PASS host proto tests\n");
    return 0;
}
