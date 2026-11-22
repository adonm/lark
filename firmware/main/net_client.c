#include "net_client.h"

#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "board.h"
#include "display.h"
#include "esp_app_desc.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "frame.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "lwip/netdb.h"
#include "lwip/sockets.h"
#include "media.h"
#include "power.h"
#include "proto.h"
#include "sdkconfig.h"
#include "telemetry.h"
#include "wifi.h"

enum {
    STATUS_MS = 5000,
    INPUT_CAP = 24,
    TCP_BUF_BYTES = 64 * 1024,
    UDP_BUF_BYTES = 16 * 1024,
    SELECT_IDLE_MS = 20,
    SELECT_BUSY_MS = 5,
    CONNECT_TIMEOUT_MS = 5000,
};

_Static_assert(FRAME_PACKET_CAP >= FRAME_HEADER_SIZE + FRAME_PIXELS * sizeof(uint16_t),
               "Receive buffer must fit full raw frames");
_Static_assert((int)AUDIO_RECORD_CHUNK_SAMPLES == (int)LARK_RTP_SAMPLES, "Record cadence must be 20 ms (320 samples)");

typedef enum {
    INPUT_TAP,
    INPUT_DRAG,
    INPUT_UP,
    TTS_ACK,
    REJECT_RECORD,
    WAKE_WORD,
} input_kind_t;

typedef struct {
    input_kind_t kind;
    uint32_t session;
    uint32_t audio_id;
    uint16_t rev_lo;
    uint16_t rec_session;
    uint8_t target;
    bool aborted;
    int x, y;
    int64_t ts_us;
} input_t;

typedef enum {
    CTRL_REC_START,
    CTRL_REC_END,
} ctrl_kind_t;

typedef struct {
    ctrl_kind_t kind;
    uint32_t session;
    uint16_t rec_session;
    uint8_t target;
    bool cancelled;
} ctrl_msg_t;

static const char *TAG = "net";

static char bridge_host[128];
static uint16_t bridge_port = LARK_DEFAULT_DEVICE_PORT;

static TaskHandle_t service;
static QueueHandle_t ctrl_queue;
static SemaphoreHandle_t hello_sem;
static portMUX_TYPE input_lock = portMUX_INITIALIZER_UNLOCKED;
static input_t inputs[INPUT_CAP], pending_drag;
static unsigned input_head, input_count, drag_after;

static portMUX_TYPE udp_lock = portMUX_INITIALIZER_UNLOCKED;
static int tcp_fd = -1;
static int udp_fd = -1;
static struct sockaddr_in bridge_udp_addr;
static uint32_t active_ssrc;
static uint16_t rtp_rec_seq;
static uint32_t rtp_rec_ts;

static uint8_t *rx_buffer;
static bool tts_accepted;
static uint8_t tts_carry;
static bool tts_odd;

static atomic_bool wifi_online;
static atomic_uint socket_session, ready_session;
static atomic_uint_fast64_t rec_ctrl_us;
static atomic_bool rec_first_rtp;
static uint32_t next_session;
static uint16_t tx_seq;
static int reconnect_ms = 1000;
static bool has_resume_token;
static uint8_t resume_token[LARK_RESUME_TOKEN_LEN];
static int64_t resume_deadline_us;
static uint32_t last_state_seq;

uint32_t net_client_session(void)
{
    return atomic_load(&socket_session);
}

static uint32_t ready_socket_session(void)
{
    uint32_t session = net_client_session();
    return atomic_load(&wifi_online) && atomic_load(&ready_session) == session ? session : 0;
}

bool net_client_connected(void)
{
    return ready_socket_session() != 0;
}

bool net_client_wait_connected(uint32_t timeout_ms)
{
    if (net_client_connected()) return true;
    if (!hello_sem) return false;
    int64_t deadline = esp_timer_get_time() + (int64_t)timeout_ms * 1000LL;
    while (!net_client_connected()) {
        int64_t rem_us = deadline - esp_timer_get_time();
        if (rem_us <= 0) break;
        TickType_t ticks = pdMS_TO_TICKS((uint32_t)((rem_us + 999) / 1000));
        if (ticks == 0) ticks = 1;
        xSemaphoreTake(hello_sem, ticks);
    }
    return net_client_connected();
}

static bool send_all_tcp(int fd, const void *data, size_t len)
{
    const uint8_t *p = data;
    size_t sent = 0;
    while (sent < len) {
        int n = lwip_send(fd, p + sent, len - sent, 0);
        if (n <= 0) return false;
        sent += (size_t)n;
    }
    return true;
}

static bool send_tcp_frame(uint32_t session, uint8_t type, const void *payload, uint32_t len, bool require_ready)
{
    if (!atomic_load(&wifi_online) || !session || session != net_client_session()) return false;
    if (require_ready && session != atomic_load(&ready_session)) return false;
    if (tcp_fd < 0 || len > LARK_PROTO_MAX_PAYLOAD) return false;

    uint8_t hdr[LARK_PROTO_HDR_SIZE];
    lark_pack_hdr(hdr, type, tx_seq++, len);
    return send_all_tcp(tcp_fd, hdr, sizeof(hdr)) && (!len || send_all_tcp(tcp_fd, payload, len));
}

static bool enqueue_session(input_t input)
{
    if (!input.session || !service) return false;
    bool full = false;
    portENTER_CRITICAL(&input_lock);
    if (input.kind == INPUT_DRAG) {
        pending_drag = input;
        drag_after = input_count;
    } else if (input_count < INPUT_CAP) {
        inputs[(input_head + input_count) % INPUT_CAP] = input;
        ++input_count;
    } else {
        full = true;
    }
    portEXIT_CRITICAL(&input_lock);
    if (full) atomic_fetch_add_explicit(&telemetry_io.input_drops, 1, memory_order_relaxed);
    return !full;
}

static bool next_input(input_t *input)
{
    bool available = true;
    portENTER_CRITICAL(&input_lock);
    if (pending_drag.session && !drag_after) {
        *input = pending_drag;
        pending_drag.session = 0;
    } else if (input_count) {
        *input = inputs[input_head];
        input_head = (input_head + 1) % INPUT_CAP;
        --input_count;
        if (drag_after) --drag_after;
    } else {
        available = false;
    }
    portEXIT_CRITICAL(&input_lock);
    return available;
}

void net_client_pointer(uint8_t kind, int x, int y, uint16_t rev_lo)
{
    uint32_t session = ready_socket_session();
    if (!session) return;
    input_kind_t ik = kind == LARK_PTR_DRAG ? INPUT_DRAG : kind == LARK_PTR_UP ? INPUT_UP : INPUT_TAP;
    enqueue_session((input_t){
        .kind = ik, .session = session, .x = x, .y = y, .rev_lo = rev_lo, .ts_us = esp_timer_get_time(),
    });
}

bool net_client_wake_word(void)
{
    return enqueue_session((input_t){ .kind = WAKE_WORD, .session = ready_socket_session() });
}

bool net_client_rec_start(uint32_t session, uint16_t rec_session, uint8_t target)
{
    if (!ctrl_queue || !session || ready_socket_session() != session) return false;
    ctrl_msg_t msg = { .kind = CTRL_REC_START, .session = session, .rec_session = rec_session, .target = target };
    if (xQueueSend(ctrl_queue, &msg, 0) != pdPASS) return false;
    if (service) xTaskNotifyGive(service);
    return true;
}

void net_client_rec_end(uint32_t session, uint16_t rec_session, uint8_t target, bool cancelled)
{
    if (!ctrl_queue || !session) return;
    ctrl_msg_t msg = {
        .kind = CTRL_REC_END,
        .session = session,
        .rec_session = rec_session,
        .target = target,
        .cancelled = cancelled,
    };
    if (xQueueSend(ctrl_queue, &msg, 0) == pdPASS && service) {
        xTaskNotifyGive(service);
    }
}

void net_client_tts_ack(uint32_t session, uint32_t audio_id, bool aborted)
{
    enqueue_session((input_t){ .kind = TTS_ACK, .session = session, .audio_id = audio_id, .aborted = aborted });
}

static bool send_rtp_audio(uint32_t session, uint8_t pt, const int16_t *pcm, size_t samples)
{
    if (!samples || samples > LARK_RTP_SAMPLES || !ready_socket_session() || session != net_client_session()) {
        return false;
    }
    uint8_t pkt[LARK_RTP_PACKET_BYTES];
    int fd = -1;
    struct sockaddr_in dst;
    uint32_t ssrc = 0, ts = 0;
    uint16_t seq = 0;

    portENTER_CRITICAL(&udp_lock);
    if (udp_fd >= 0 && active_ssrc != 0 && bridge_udp_addr.sin_port != 0) {
        fd = udp_fd;
        dst = bridge_udp_addr;
        ssrc = active_ssrc;
        seq = rtp_rec_seq++;
        ts = rtp_rec_ts;
        rtp_rec_ts += (uint32_t)samples;
    }
    portEXIT_CRITICAL(&udp_lock);

    if (fd < 0) return false;
    size_t bytes = samples * sizeof(int16_t);
    lark_rtp_encode(pkt, pt, seq, ts, ssrc);
    memcpy(pkt + LARK_RTP_HDR_SIZE, pcm, bytes);
    int sent = lwip_sendto(fd, pkt, LARK_RTP_HDR_SIZE + bytes, 0, (const struct sockaddr *)&dst, sizeof(dst));
    if (sent == (int)(LARK_RTP_HDR_SIZE + bytes) &&
        atomic_exchange_explicit(&rec_first_rtp, false, memory_order_relaxed)) {
        uint64_t dt = (uint64_t)esp_timer_get_time() - atomic_load_explicit(&rec_ctrl_us, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.rec_start_us, (unsigned)dt, memory_order_relaxed);
    }
    return sent == (int)(LARK_RTP_HDR_SIZE + bytes);
}

bool net_client_rec_pcm(uint32_t session, const int16_t *pcm, size_t samples)
{
    return send_rtp_audio(session, LARK_RTP_PT_REC_PCM, pcm, samples);
}

static void send_input(const input_t *input)
{
    uint32_t session = input->session;
    if (session != atomic_load(&ready_session)) return;

    if (input->kind == TTS_ACK) {
        lark_tts_ack_t ack = {
            .aborted = input->aborted ? 1 : 0,
            .audio_id = (uint64_t)input->audio_id,
        };
        send_tcp_frame(session, LARK_MSG_TTS_ACK, &ack, sizeof(ack), true);
        return;
    }
    if (input->kind == REJECT_RECORD) {
        net_client_rec_end(session, input->rec_session, input->target, true);
        return;
    }
    if (input->kind == WAKE_WORD) {
        send_tcp_frame(session, LARK_MSG_WAKE_WORD, NULL, 0, true);
        return;
    }
    lark_pointer_t ptr = {
        .kind = input->kind == INPUT_DRAG ? LARK_PTR_DRAG : input->kind == INPUT_UP ? LARK_PTR_UP : LARK_PTR_TAP,
        .pad = 0,
        .x = (uint16_t)input->x,
        .y = (uint16_t)input->y,
        .rev_lo = input->rev_lo,
    };
    if (send_tcp_frame(session, LARK_MSG_POINTER, &ptr, sizeof(ptr), true) && input->ts_us) {
        uint64_t dt = (uint64_t)esp_timer_get_time() - (uint64_t)input->ts_us;
        atomic_store_explicit(&telemetry_io.ptr_send_us, (unsigned)dt, memory_order_relaxed);
    }
}

static bool send_hello(uint32_t session, uint16_t local_udp_port)
{
    size_t id_len = strlen(device_id);
    if (has_resume_token && esp_timer_get_time() < resume_deadline_us && id_len <= 255) {
        uint8_t rbuf[sizeof(lark_resume_t) + 64];
        if (sizeof(lark_resume_t) + id_len <= sizeof(rbuf)) {
            lark_resume_t r = {
                .proto_version = LARK_PROTO_VERSION,
                .last_seq = last_state_seq,
                .id_len = (uint8_t)id_len,
            };
            memcpy(r.resume_token, resume_token, LARK_RESUME_TOKEN_LEN);
            memcpy(rbuf, &r, sizeof(r));
            memcpy(rbuf + sizeof(r), device_id, id_len);
            return send_tcp_frame(session, LARK_MSG_RESUME, rbuf, (uint32_t)(sizeof(r) + id_len), false);
        }
    }
    uint32_t caps = LARK_CAP_DRAG | LARK_CAP_POINTER_REV;
    if (BOARD_HAS_BATTERY) caps |= LARK_CAP_BATTERY | LARK_CAP_PANEL_SLEEP;
    if (media_audio_ready()) {
        caps |= LARK_CAP_BRIDGE_ENDPOINT | LARK_CAP_WAKE_WORD;
    }
    const char *board = BOARD_NAME;
    const char *fw = esp_app_get_description()->version;
    const char *tok = CONFIG_LARK_TOKEN;
    size_t board_len = strlen(board);
    size_t fw_len = strlen(fw);
    size_t tok_len = strlen(tok);
    if (id_len > 255 || board_len > 255 || fw_len > 255 || tok_len > 255) return false;

    uint8_t buf[sizeof(lark_hello_t) + 512];
    lark_hello_t hello = {
        .proto_version = LARK_PROTO_VERSION,
        .kind = LARK_KIND_HARDWARE,
        .caps_mask = caps,
        .display_formats = LARK_DISP_RGB565,
        .frame_codecs = LARK_CODEC_RAW | LARK_CODEC_RLE565 | LARK_CODEC_LZ4 | LARK_CODEC_JPEG,
        .width = FRAME_WIDTH,
        .height = FRAME_HEIGHT,
#ifdef LARK_BOX3
        .shape = LARK_SHAPE_RECT,
#else
        .shape = LARK_SHAPE_ROUND,
#endif
        .flags = 0,
        .udp_port = local_udp_port,
        .id_len = (uint8_t)id_len,
        .comp_len = 0,
        .board_len = (uint8_t)board_len,
        .fw_len = (uint8_t)fw_len,
        .tok_len = (uint8_t)tok_len,
    };
    size_t total = lark_encode_hello(buf, sizeof(buf), &hello, device_id, "", board, fw, tok);
    if (!total) return false;
    return send_tcp_frame(session, LARK_MSG_HELLO, buf, (uint32_t)total, false);
}

static void send_status(uint32_t session)
{
    if (media_is_recording()) return;
    telemetry_t hw = telemetry_read();
    battery_status_t battery = battery_status();
    lark_telemetry_t st = {
        .wifi_rssi = (int16_t)hw.wifi_rssi,
        .battery_pct = (int16_t)(BOARD_HAS_BATTERY ? battery.pct : -1),
        .charging = (uint8_t)(BOARD_HAS_BATTERY && battery.charging ? 1 : 0),
        .cpu_pct = (uint8_t)(hw.cpu_pct > 100 ? 100 : hw.cpu_pct),
        .flags = (uint8_t)(display_is_sleeping() ? LARK_TELEMETRY_SLEEPING : 0),
        .pad = 0,
        .free_heap = (uint32_t)hw.free_heap,
        .uptime_s = (uint32_t)hw.uptime_s,
    };
    send_tcp_frame(session, LARK_MSG_TELEMETRY, &st, sizeof(st), true);
}

static void receive_tts_pcm(const uint8_t *bytes, size_t len)
{
    if (!len || !tts_accepted) return;
    if (tts_odd) {
        uint8_t sample[2] = { tts_carry, *bytes++ };
        --len;
        tts_odd = false;
        if (!media_tts_pcm(sample, sizeof(sample))) {
            tts_accepted = false;
            return;
        }
    }
    size_t even = len & ~(size_t)1;
    if (even && !media_tts_pcm(bytes, even)) {
        tts_accepted = false;
        return;
    }
    if (len != even) {
        tts_carry = bytes[even];
        tts_odd = true;
    }
}

static void handle_tcp_message(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    switch (type) {
    case LARK_MSG_WELCOME: {
        lark_welcome_t w;
        char tid[32], cid[32];
        if (!lark_decode_welcome(payload, len, &w, tid, sizeof(tid), cid, sizeof(cid))) break;
        memcpy(resume_token, w.resume_token, LARK_RESUME_TOKEN_LEN);
        has_resume_token = true;
        resume_deadline_us = esp_timer_get_time() + (int64_t)w.resume_window_ms * 1000LL;
        last_state_seq = w.seq;
        portENTER_CRITICAL(&udp_lock);
        bridge_udp_addr.sin_port = htons(w.udp_port);
        active_ssrc = w.ssrc;
        rtp_rec_seq = 0;
        rtp_rec_ts = 0;
        portEXIT_CRITICAL(&udp_lock);
        atomic_store(&ready_session, session);
        if (hello_sem) xSemaphoreGive(hello_sem);
        display_set_link(true);
        ESP_LOGI(TAG, "Bridge welcome (id=%s comp=%s udp=%u ssrc=0x%08" PRIx32 " seq=%" PRIu32 ")",
                 tid, cid, w.udp_port, w.ssrc, w.seq);
        break;
    }
    case LARK_MSG_REJECT: {
        uint8_t reason = 0;
        char msg[128];
        if (!lark_decode_reject(payload, len, &reason, msg, sizeof(msg))) break;
        ESP_LOGW(TAG, "Bridge rejected connection (reason=%u: %s)", reason, msg);
        if (reason == LARK_REJECT_UNKNOWN_TERMINAL) {
            has_resume_token = false;
        }
        break;
    }
    case LARK_MSG_FRAME:
        if (!display_apply_frame(payload, len)) ESP_LOGD(TAG, "Ignored display frame");
        break;
    case LARK_MSG_WAKE_WORD_CTRL:
        if (len >= 1 && net_client_connected()) media_set_wake_word(payload[0] != 0);
        break;
    case LARK_MSG_REC_CTRL: {
        if (len < sizeof(lark_rec_ctrl_t)) break;
        lark_rec_ctrl_t ctrl;
        memcpy(&ctrl, payload, sizeof(ctrl));
        if (ctrl.op == LARK_REC_STOP || ctrl.op == LARK_REC_CANCEL) {
            media_record_stop(ctrl.op == LARK_REC_CANCEL);
        } else if (ctrl.op == LARK_REC_START) {
            atomic_store_explicit(&rec_ctrl_us, (uint64_t)esp_timer_get_time(), memory_order_relaxed);
            atomic_store_explicit(&rec_first_rtp, true, memory_order_relaxed);
            uint8_t target = ctrl.flags & LARK_REC_TARGET_INTERCOM;
            bool bridge_ep = (ctrl.flags & LARK_REC_TARGET_BRIDGE_EP) != 0;
            bool listen = (ctrl.flags & LARK_REC_TARGET_LISTEN) != 0;
            if (!media_record_start(ctrl.session, target, bridge_ep, listen)) {
                enqueue_session((input_t){
                    .kind = REJECT_RECORD,
                    .session = session,
                    .rec_session = ctrl.session,
                    .target = target,
                });
            }
        }
        break;
    }
    case LARK_MSG_TTS_CTRL: {
        if (len < sizeof(lark_tts_ctrl_t)) break;
        lark_tts_ctrl_t ctrl;
        memcpy(&ctrl, payload, sizeof(ctrl));
        uint32_t audio_id = (uint32_t)ctrl.audio_id;
        if (ctrl.op == LARK_TTS_START) {
            tts_odd = false;
            tts_accepted = media_tts_start(audio_id);
            if (!tts_accepted) net_client_tts_ack(session, audio_id, true);
        } else if (ctrl.op == LARK_TTS_END) {
            if (tts_accepted) {
                if (tts_odd) {
                    media_interrupt();
                } else {
                    media_tts_end();
                }
            }
            tts_accepted = false;
            tts_odd = false;
        } else {
            media_interrupt();
            tts_accepted = false;
            tts_odd = false;
        }
        break;
    }
    case LARK_MSG_TTS_PCM:
        receive_tts_pcm(payload, len);
        break;
    case LARK_MSG_CHIME: {
        if (len < 1) break;
        static const audio_tone_t map[] = {
            [LARK_CHIME_TAP] = AUDIO_START_TONE,
            [LARK_CHIME_THINK] = AUDIO_THINK_TONE,
            [LARK_CHIME_CANCEL] = AUDIO_QUIET_TONE,
            [LARK_CHIME_INTERCOM] = AUDIO_HAPPY_TONE,
            [LARK_CHIME_RING] = AUDIO_RING_TONE,
        };
        if (payload[0] < sizeof(map) / sizeof(map[0])) media_chime(map[payload[0]]);
        break;
    }
    case LARK_MSG_POWER:
        if (len >= 1) {
            display_set_brightness((int)payload[0]);
        }
        break;
    case LARK_MSG_VOLUME:
        if (len >= 1) {
            int level = payload[0];
            bool persist = len >= 2 ? payload[1] != 0 : true;
            if (level <= 100) {
                esp_err_t err = persist ? audio_set_volume(level) : audio_set_volume_live(level);
                if (err != ESP_OK) ESP_LOGW(TAG, "Could not set volume");
            }
        }
        break;
    case LARK_MSG_STATUS: {
        if (len < sizeof(lark_status_t)) break;
        lark_status_t st;
        memcpy(&st, payload, sizeof(st));
        if (st.seq >= last_state_seq) {
            last_state_seq = st.seq;
            ESP_LOGD(TAG, "Status seq=%" PRIu32 " state=%u progress=%u kind=%u", st.seq, st.state, st.progress, st.kind);
        }
        break;
    }
    case LARK_MSG_VOICE_OWNER: {
        if (len >= 4) {
            uint32_t seq = (uint32_t)payload[0] | ((uint32_t)payload[1] << 8) |
                           ((uint32_t)payload[2] << 16) | ((uint32_t)payload[3] << 24);
            if (seq >= last_state_seq) last_state_seq = seq;
        }
        break;
    }
    case LARK_MSG_PING:
        if (len == 4) send_tcp_frame(session, LARK_MSG_PONG, payload, 4, false);
        break;
    default:
        break;
    }
}

static bool recv_exact(int fd, uint8_t *dst, size_t len)
{
    size_t got = 0;
    while (got < len) {
        if (!atomic_load(&wifi_online)) return false;
        int n = lwip_recv(fd, dst + got, len - got, 0);
        if (n > 0) {
            got += (size_t)n;
            continue;
        }
        if (n == 0) return false;
        if (errno == EINTR) continue;
        if (errno == EAGAIN || errno == EWOULDBLOCK) {
            int waited = 0;
            while (waited < 5000) {
                if (!atomic_load(&wifi_online)) return false;
                fd_set rfds;
                FD_ZERO(&rfds);
                FD_SET(fd, &rfds);
                struct timeval tv = { .tv_sec = 0, .tv_usec = 200000 };
                int rc = lwip_select(fd + 1, &rfds, NULL, NULL, &tv);
                if (rc > 0) break;
                if (rc < 0) return false;
                waited += 200;
            }
            if (waited >= 5000) return false;
            continue;
        }
        return false;
    }
    return true;
}

static void close_sockets(void)
{
    int tfd = tcp_fd;
    tcp_fd = -1;
    if (tfd >= 0) lwip_close(tfd);

    portENTER_CRITICAL(&udp_lock);
    int ufd = udp_fd;
    udp_fd = -1;
    active_ssrc = 0;
    memset(&bridge_udp_addr, 0, sizeof(bridge_udp_addr));
    portEXIT_CRITICAL(&udp_lock);
    if (ufd >= 0) lwip_close(ufd);

    if (atomic_exchange(&socket_session, 0)) {
        atomic_store(&ready_session, 0);
        tts_accepted = false;
        tts_odd = false;
        media_link_lost();
        display_set_link(false);
    }
}

static bool open_connection(uint32_t *out_session)
{
    char port_str[8];
    snprintf(port_str, sizeof(port_str), "%u", bridge_port);
    struct addrinfo hints = { .ai_family = AF_INET, .ai_socktype = SOCK_STREAM };
    struct addrinfo *res = NULL;
    if (lwip_getaddrinfo(bridge_host, port_str, &hints, &res) != 0 || !res) {
        return false;
    }
    struct sockaddr_in remote = *(const struct sockaddr_in *)res->ai_addr;
    lwip_freeaddrinfo(res);

    int tfd = lwip_socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    int ufd = lwip_socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (tfd < 0 || ufd < 0) {
        if (tfd >= 0) lwip_close(tfd);
        if (ufd >= 0) lwip_close(ufd);
        return false;
    }

    int one = 1;
    int tcp_buf = TCP_BUF_BYTES;
    int udp_buf = UDP_BUF_BYTES;
    lwip_setsockopt(tfd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
    lwip_setsockopt(tfd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof(one));
    lwip_setsockopt(tfd, SOL_SOCKET, SO_RCVBUF, &tcp_buf, sizeof(tcp_buf));
    lwip_setsockopt(tfd, SOL_SOCKET, SO_SNDBUF, &tcp_buf, sizeof(tcp_buf));
    lwip_setsockopt(ufd, SOL_SOCKET, SO_RCVBUF, &udp_buf, sizeof(udp_buf));
    lwip_setsockopt(ufd, SOL_SOCKET, SO_SNDBUF, &udp_buf, sizeof(udp_buf));

    struct sockaddr_in bind_addr = { .sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_ANY), .sin_port = 0 };
    if (lwip_bind(ufd, (struct sockaddr *)&bind_addr, sizeof(bind_addr)) != 0) {
        lwip_close(tfd);
        lwip_close(ufd);
        return false;
    }
    socklen_t blen = sizeof(bind_addr);
    if (lwip_getsockname(ufd, (struct sockaddr *)&bind_addr, &blen) != 0) {
        lwip_close(tfd);
        lwip_close(ufd);
        return false;
    }
    uint16_t local_udp_port = ntohs(bind_addr.sin_port);

    int flags = lwip_fcntl(tfd, F_GETFL, 0);
    lwip_fcntl(tfd, F_SETFL, flags | O_NONBLOCK);
    int rc = lwip_connect(tfd, (const struct sockaddr *)&remote, sizeof(remote));
    if (rc < 0 && errno != EINPROGRESS) {
        lwip_close(tfd);
        lwip_close(ufd);
        return false;
    }
    if (rc < 0) {
        fd_set wfds;
        FD_ZERO(&wfds);
        FD_SET(tfd, &wfds);
        struct timeval tv = { .tv_sec = CONNECT_TIMEOUT_MS / 1000, .tv_usec = 0 };
        if (lwip_select(tfd + 1, NULL, &wfds, NULL, &tv) <= 0) {
            lwip_close(tfd);
            lwip_close(ufd);
            return false;
        }
        int err = 0;
        socklen_t elen = sizeof(err);
        if (lwip_getsockopt(tfd, SOL_SOCKET, SO_ERROR, &err, &elen) != 0 || err != 0) {
            lwip_close(tfd);
            lwip_close(ufd);
            return false;
        }
    }
    lwip_fcntl(tfd, F_SETFL, flags);
    struct timeval snd_tv = { .tv_sec = 0, .tv_usec = 500000 };
    lwip_setsockopt(tfd, SOL_SOCKET, SO_SNDTIMEO, &snd_tv, sizeof(snd_tv));
    lwip_fcntl(ufd, F_SETFL, lwip_fcntl(ufd, F_GETFL, 0) | O_NONBLOCK);

    if (++next_session == 0) ++next_session;
    uint32_t session = next_session;

    tcp_fd = tfd;
    tx_seq = 0;

    portENTER_CRITICAL(&udp_lock);
    udp_fd = ufd;
    bridge_udp_addr = remote;
    bridge_udp_addr.sin_port = 0;
    active_ssrc = 0;
    portEXIT_CRITICAL(&udp_lock);

    atomic_store(&socket_session, session);
    if (!send_hello(session, local_udp_port)) {
        close_sockets();
        return false;
    }
    *out_session = session;
    return true;
}

static bool has_pending_inputs(void)
{
    portENTER_CRITICAL(&input_lock);
    bool any = input_count > 0 || pending_drag.session != 0;
    portEXIT_CRITICAL(&input_lock);
    return any || (ctrl_queue && uxQueueMessagesWaiting(ctrl_queue) > 0);
}

static void drain_ctrl(void)
{
    ctrl_msg_t msg;
    while (ctrl_queue && xQueueReceive(ctrl_queue, &msg, 0) == pdPASS) {
        if (msg.kind == CTRL_REC_START) {
            uint8_t wire[3] = {
                (uint8_t)(msg.rec_session & 0xFF),
                (uint8_t)(msg.rec_session >> 8),
                0,
            };
            send_tcp_frame(msg.session, LARK_MSG_REC_START, wire, sizeof(wire), true);
        } else if (msg.kind == CTRL_REC_END) {
            uint8_t wire[4] = {
                (uint8_t)(msg.rec_session & 0xFF),
                (uint8_t)(msg.rec_session >> 8),
                msg.cancelled ? 1 : 0,
                0,
            };
            send_tcp_frame(msg.session, LARK_MSG_REC_END, wire, sizeof(wire), true);
        }
    }
}

static void service_task(void *arg)
{
    (void)arg;
    for (;;) {
        if (!atomic_load(&wifi_online)) {
            close_sockets();
            reconnect_ms = 1000;
            while (!atomic_load(&wifi_online)) {
                ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
            }
            continue;
        }
        uint32_t session = 0;
        if (!open_connection(&session)) {
            close_sockets();
            ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(reconnect_ms));
            if (reconnect_ms < 30000) reconnect_ms = reconnect_ms > 15000 ? 30000 : reconnect_ms * 2;
            continue;
        }
        reconnect_ms = 1000;
        int64_t after_status = esp_timer_get_time() / 1000 + STATUS_MS;

        while (atomic_load(&wifi_online) && tcp_fd >= 0) {
            drain_ctrl();
            input_t input;
            while (next_input(&input)) send_input(&input);

            int64_t now = esp_timer_get_time() / 1000;
            if (ready_socket_session() && now >= after_status) {
                send_status(session);
                after_status = now + STATUS_MS;
            }

            fd_set rfds;
            FD_ZERO(&rfds);
            FD_SET(tcp_fd, &rfds);
            int wait_ms = has_pending_inputs() ? SELECT_BUSY_MS : SELECT_IDLE_MS;
            struct timeval tv = { .tv_sec = 0, .tv_usec = wait_ms * 1000 };
            int ready = lwip_select(tcp_fd + 1, &rfds, NULL, NULL, &tv);
            if (ready < 0) break;

            if (FD_ISSET(tcp_fd, &rfds)) {
                uint8_t hbuf[LARK_PROTO_HDR_SIZE];
                if (!recv_exact(tcp_fd, hbuf, sizeof(hbuf))) break;
                lark_hdr_t hdr;
                if (!lark_unpack_hdr(hbuf, &hdr)) {
                    ESP_LOGW(TAG, "Invalid TCP frame header");
                    break;
                }
                if (hdr.length <= FRAME_PACKET_CAP) {
                    if (hdr.length && !recv_exact(tcp_fd, rx_buffer, hdr.length)) break;
                    handle_tcp_message(session, hdr.type, rx_buffer, hdr.length);
                } else {
                    // Drain oversized non-frame payloads cleanly in chunks.
                    uint32_t rem = hdr.length;
                    bool ok = true;
                    while (rem > 0) {
                        uint32_t chunk = rem > FRAME_PACKET_CAP ? FRAME_PACKET_CAP : rem;
                        if (!recv_exact(tcp_fd, rx_buffer, chunk)) {
                            ok = false;
                            break;
                        }
                        if (hdr.type == LARK_MSG_TTS_PCM) receive_tts_pcm(rx_buffer, chunk);
                        rem -= chunk;
                    }
                    if (!ok) break;
                }
            }
        }
        close_sockets();
        if (atomic_load(&wifi_online)) {
            ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(500));
        }
    }
}

void net_client_network_changed(bool online)
{
    atomic_store(&wifi_online, online);
    if (service) xTaskNotifyGive(service);
}

esp_err_t net_client_init(void)
{
    if (!lark_parse_bridge_url(CONFIG_LARK_URL, bridge_host, sizeof(bridge_host), &bridge_port)) {
        return ESP_ERR_INVALID_ARG;
    }
    rx_buffer = heap_caps_malloc(FRAME_PACKET_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    ctrl_queue = xQueueCreate(16, sizeof(ctrl_msg_t));
    hello_sem = xSemaphoreCreateBinary();
    if (!rx_buffer || !ctrl_queue || !hello_sem) {
        free(rx_buffer);
        rx_buffer = NULL;
        if (ctrl_queue) vQueueDelete(ctrl_queue);
        ctrl_queue = NULL;
        if (hello_sem) vSemaphoreDelete(hello_sem);
        hello_sem = NULL;
        return ESP_ERR_NO_MEM;
    }
    if (xTaskCreate(service_task, "net_service", 6144, NULL, 5, &service) != pdPASS) {
        free(rx_buffer);
        rx_buffer = NULL;
        return ESP_ERR_NO_MEM;
    }
    ESP_LOGI(TAG, "Native Lark device client targeting %s:%u", bridge_host, bridge_port);
    return ESP_OK;
}
