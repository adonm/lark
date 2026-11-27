#include "net_client.h"

#include <errno.h>
#include <inttypes.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include "audio_logic.h"
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
#include "lwip/sockets.h"
#include "media.h"
#include "net_logic.h"
#include "net_transport.h"
#include "power.h"
#include "proto.h"
#include "sdkconfig.h"
#include "telemetry.h"
#include "telemetry_logic.h"
#include "wifi.h"

enum {
    WAKE_POLL_MS = 50,
    ACK_RETRY_MS = 20,
    INPUT_CAP = 24,
};

_Static_assert(FRAME_PACKET_CAP >= FRAME_HEADER_SIZE + FRAME_PIXELS * sizeof(uint16_t),
               "Receive buffer must fit full raw frames");
_Static_assert((int)AUDIO_RECORD_CHUNK_SAMPLES == (int)LARK_RTP_SAMPLES, "Record cadence must be 20 ms (320 samples)");

typedef enum {
    INPUT_TAP,
    INPUT_DRAG,
    INPUT_UP,
    REJECT_RECORD,
} input_kind_t;

typedef struct {
    input_kind_t kind;
    uint32_t session;
    uint16_t rev_lo;
    uint16_t rec_session;
    uint8_t target;
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

static char edge_host[128];
static uint16_t edge_port = LARK_DEFAULT_DEVICE_PORT;

static TaskHandle_t service;
static QueueHandle_t ctrl_queue;
static SemaphoreHandle_t hello_sem;
static portMUX_TYPE input_lock = portMUX_INITIALIZER_UNLOCKED;
static input_t inputs[INPUT_CAP], pending_drag;
static unsigned input_head, input_count, drag_after;

static uint8_t *rx_buffer;
static bool tts_accepted;
static audio_tts_splitter_t tts_split;

static atomic_bool wifi_online;
static atomic_uint socket_session, ready_session;
static atomic_uint_fast64_t rec_ctrl_us;
static atomic_bool rec_first_rtp;
static uint32_t next_session;
static uint32_t tcp_sessions;
static net_backoff_t backoff;
static bool session_rejected;
static portMUX_TYPE ack_lock = portMUX_INITIALIZER_UNLOCKED;
static net_ack_t pending_ack;
static bool has_resume_token;
static uint8_t resume_token[LARK_RESUME_TOKEN_LEN];
static int64_t resume_deadline_us;

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

static bool send_tcp_frame(uint32_t session, uint8_t type, const void *payload, uint32_t len, bool require_ready)
{
    if (!atomic_load(&wifi_online) || !session || session != net_client_session()) return false;
    if (require_ready && session != atomic_load(&ready_session)) return false;
    return net_transport_send(type, payload, len);
}

static void wake_service(void)
{
    if (service) xTaskNotifyGive(service);
    net_transport_kick();
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
    else wake_service();
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

bool net_client_rec_start(uint32_t session, uint16_t rec_session, uint8_t target)
{
    if (!ctrl_queue || !session || ready_socket_session() != session) return false;
    ctrl_msg_t msg = { .kind = CTRL_REC_START, .session = session, .rec_session = rec_session, .target = target };
    if (xQueueSend(ctrl_queue, &msg, 0) != pdPASS) return false;
    wake_service();
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
    if (xQueueSend(ctrl_queue, &msg, 0) == pdPASS) {
        wake_service();
    }
}

void net_client_tts_ack(uint32_t session, uint32_t audio_id, bool aborted)
{
    if (!session || !audio_id) return;
    // Acks share no ring with pointer input: the newest stream replaces an
    // unsent ack, and the service loop retries until the frame is on the wire.
    portENTER_CRITICAL(&ack_lock);
    net_ack_submit(&pending_ack, session, audio_id, aborted);
    portEXIT_CRITICAL(&ack_lock);
    wake_service();
}

static bool drain_ack_retry(void)
{
    uint32_t ready = ready_socket_session();
    net_ack_t ack;
    portENTER_CRITICAL(&ack_lock);
    if (pending_ack.pending && pending_ack.session != ready) {
        // The stream or socket it belonged to is gone; it can never be sent.
        pending_ack.pending = false;
    }
    bool pending = net_ack_take(&pending_ack, &ack);
    portEXIT_CRITICAL(&ack_lock);
    if (!pending) return false;

    lark_tts_ack_t wire = { .aborted = ack.aborted ? 1 : 0, .audio_id = (uint64_t)ack.audio_id };
    if (send_tcp_frame(ack.session, LARK_MSG_TTS_ACK, &wire, sizeof(wire), true)) {
        portENTER_CRITICAL(&ack_lock);
        net_ack_sent(&pending_ack);
        portEXIT_CRITICAL(&ack_lock);
        return false;
    }
    return true;
}

int16_t *net_client_rec_buf(void)
{
    return net_transport_rtp_buf();
}

static bool send_rtp_audio(uint32_t session, uint8_t pt, const int16_t *pcm, size_t samples)
{
    if (!samples || samples > LARK_RTP_SAMPLES || !ready_socket_session() || session != net_client_session()) {
        return false;
    }
    bool sent = net_transport_send_rtp(pt, pcm, samples);
    if (sent && atomic_exchange_explicit(&rec_first_rtp, false, memory_order_relaxed)) {
        uint64_t dt = (uint64_t)esp_timer_get_time() - atomic_load_explicit(&rec_ctrl_us, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.rec_start_us, (unsigned)dt, memory_order_relaxed);
    }
    return sent;
}

bool net_client_rec_pcm(uint32_t session, const int16_t *pcm, size_t samples)
{
    return send_rtp_audio(session, LARK_RTP_PT_REC_PCM, pcm, samples);
}

static void send_input(const input_t *input)
{
    uint32_t session = input->session;
    if (session != atomic_load(&ready_session)) return;

    if (input->kind == REJECT_RECORD) {
        net_client_rec_end(session, input->rec_session, input->target, true);
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
        caps |= LARK_CAP_EDGE_ENDPOINT;
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
        // Lets the edge slow its keepalive and the brain avoid waking a
        // sleeping panel for every status tick.
        .flags = display_is_sleeping() ? LARK_TELEMETRY_SLEEPING : 0,
        .pad = 0,
        .free_heap = (uint32_t)hw.free_heap,
        .uptime_s = (uint32_t)hw.uptime_s,
    };
    send_tcp_frame(session, LARK_MSG_TELEMETRY, &st, sizeof(st), true);
}

static bool tts_pcm_sink(const void *data, size_t len, void *ctx)
{
    (void)ctx;
    return media_tts_pcm(data, len);
}

static void receive_tts_pcm(const uint8_t *bytes, size_t len)
{
    if (!len || !tts_accepted) return;
    if (!audio_tts_feed(&tts_split, bytes, len, tts_pcm_sink, NULL)) {
        tts_accepted = false;
    }
}

static void handle_handshake(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    if (type == LARK_MSG_WELCOME) {
        lark_welcome_t w;
        char tid[32], cid[32];
        if (!lark_decode_welcome(payload, len, &w, tid, sizeof(tid), cid, sizeof(cid))) return;
        memcpy(resume_token, w.resume_token, LARK_RESUME_TOKEN_LEN);
        has_resume_token = true;
        resume_deadline_us = esp_timer_get_time() + (int64_t)w.resume_window_ms * 1000LL;
        net_transport_set_udp_peer(w.udp_port, w.ssrc);
        atomic_store(&ready_session, session);
        net_backoff_connected(&backoff);
        if (hello_sem) xSemaphoreGive(hello_sem);
        display_set_link(true);
        ESP_LOGI(TAG, "Edge welcome (id=%s comp=%s udp=%u ssrc=0x%08" PRIx32 ")",
                 tid, cid, w.udp_port, w.ssrc);
        return;
    }

    lark_reject_t reject;
    char msg[128];
    if (!lark_decode_reject(payload, len, &reject, msg, sizeof(msg))) return;
    ESP_LOGW(TAG, "Edge rejected connection (reason=%u: %s)", reject.reason, msg);
    session_rejected = true;
    if (reject.reason == LARK_REJECT_UNKNOWN_TERMINAL) {
        has_resume_token = false;
    }
    // Bad token or protocol version keeps retrying with bounded backoff: a
    // transient server misconfiguration heals without a physical power cycle.
}

static void handle_display(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    (void)session;
    switch (type) {
    case LARK_MSG_FRAME:
        if (!display_apply_frame(payload, len)) ESP_LOGD(TAG, "Ignored display frame");
        break;
    case LARK_MSG_POWER:
        if (len >= 1) display_set_brightness((int)payload[0]);
        break;
    default:
        break;
    }
}

static void handle_recording(uint32_t session, const uint8_t *payload, uint32_t len)
{
    lark_rec_ctrl_t ctrl;
    char target[32];
    if (!lark_decode_rec_ctrl(payload, len, &ctrl, target, sizeof(target))) return;
    if (ctrl.op == LARK_REC_STOP || ctrl.op == LARK_REC_CANCEL) {
        media_record_stop(ctrl.op == LARK_REC_CANCEL);
    } else if (ctrl.op == LARK_REC_START) {
        atomic_store_explicit(&rec_ctrl_us, (uint64_t)esp_timer_get_time(), memory_order_relaxed);
        atomic_store_explicit(&rec_first_rtp, true, memory_order_relaxed);
        uint8_t target_id = ctrl.flags & LARK_REC_TARGET_INTERCOM;
        bool edge_ep = (ctrl.flags & LARK_REC_TARGET_EDGE_EP) != 0;
        bool listen = (ctrl.flags & LARK_REC_TARGET_LISTEN) != 0;
        if (!media_record_start(ctrl.session, target_id, edge_ep, listen)) {
            enqueue_session((input_t){
                .kind = REJECT_RECORD,
                .session = session,
                .rec_session = ctrl.session,
                .target = target_id,
            });
        }
    }
}

static void handle_tts(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    if (type == LARK_MSG_TTS_PCM) {
        receive_tts_pcm(payload, len);
        return;
    }

    lark_tts_ctrl_t ctrl;
    if (!lark_decode_tts_ctrl(payload, len, &ctrl)) return;
    uint32_t audio_id = (uint32_t)ctrl.audio_id;
    if (ctrl.op == LARK_TTS_START) {
        tts_split = (audio_tts_splitter_t){ 0 };
        tts_accepted = media_tts_start(audio_id);
        if (!tts_accepted) net_client_tts_ack(session, audio_id, true);
    } else if (ctrl.op == LARK_TTS_END) {
        // Always close the stream: an overflow or interrupt may have stopped
        // accepting PCM, but the stream still owes its one completion ack.
        media_tts_end();
        tts_accepted = false;
        tts_split = (audio_tts_splitter_t){ 0 };
    } else {
        media_interrupt();
        tts_accepted = false;
        tts_split = (audio_tts_splitter_t){ 0 };
    }
}

static void handle_misc(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    switch (type) {
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
        // Only the state byte is meaningful; the remaining bytes are reserved
        // padding and the decoder rejects a payload that does not match it.
        lark_status_t status;
        if (!lark_decode_status(payload, len, &status)) break;
        ESP_LOGD(TAG, "Status state=%u", status.state);
        break;
    }
    case LARK_MSG_PING:
        if (len == 4) send_tcp_frame(session, LARK_MSG_PONG, payload, 4, false);
        break;
    default:
        break;
    }
}

static void handle_tcp_message(uint32_t session, uint8_t type, const uint8_t *payload, uint32_t len)
{
    switch (type) {
    case LARK_MSG_WELCOME:
    case LARK_MSG_REJECT:
        handle_handshake(session, type, payload, len);
        break;
    case LARK_MSG_FRAME:
    case LARK_MSG_POWER:
        handle_display(session, type, payload, len);
        break;
    case LARK_MSG_REC_CTRL:
        handle_recording(session, payload, len);
        break;
    case LARK_MSG_TTS_CTRL:
    case LARK_MSG_TTS_PCM:
        handle_tts(session, type, payload, len);
        break;
    default:
        handle_misc(session, type, payload, len);
        break;
    }
}

static void close_sockets(void)
{
    net_transport_close();
    if (atomic_exchange(&socket_session, 0)) {
        atomic_store(&ready_session, 0);
        tts_accepted = false;
        tts_split = (audio_tts_splitter_t){ 0 };
        media_link_lost();
        display_set_link(false);
    }
}

static bool open_connection(uint32_t *out_session)
{
    struct sockaddr_in remote;
    if (!net_transport_resolve(edge_host, edge_port, &remote)) return false;

    uint16_t local_udp_port = 0;
    if (!net_transport_open(&remote, &local_udp_port)) return false;

    if (++next_session == 0) ++next_session;
    uint32_t session = next_session;
    session_rejected = false;

    atomic_store(&socket_session, session);
    if (!send_hello(session, local_udp_port)) {
        close_sockets();
        return false;
    }
    if (tcp_sessions++) {
        atomic_fetch_add_explicit(&telemetry_io.reconnects, 1, memory_order_relaxed);
    }
    *out_session = session;
    return true;
}

static void drain_ctrl(void)
{
    ctrl_msg_t msg;
    while (ctrl_queue && xQueueReceive(ctrl_queue, &msg, 0) == pdPASS) {
        if (msg.kind == CTRL_REC_START) {
            lark_rec_start_t wire = { .session = msg.rec_session, .target_len = 0 };
            send_tcp_frame(msg.session, LARK_MSG_REC_START, &wire, sizeof(wire), true);
        } else if (msg.kind == CTRL_REC_END) {
            lark_rec_end_t wire = {
                .session = msg.rec_session,
                .cancelled = msg.cancelled ? 1 : 0,
                .target_len = 0,
            };
            send_tcp_frame(msg.session, LARK_MSG_REC_END, &wire, sizeof(wire), true);
        }
    }
}

static void run_session(uint32_t session)
{
    int interval = telemetry_status_interval_ms(display_is_sleeping());
    int64_t after_status = esp_timer_get_time() / 1000 + interval;

    while (atomic_load(&wifi_online) && net_transport_tcp_fd() >= 0 && !net_transport_broken()) {
        net_transport_drain_wake();
        drain_ctrl();
        input_t input;
        while (next_input(&input)) send_input(&input);
        bool ack_waiting = drain_ack_retry();

        int64_t now = esp_timer_get_time() / 1000;
        if (ready_socket_session() && now >= after_status) {
            send_status(session);
            interval = telemetry_status_interval_ms(display_is_sleeping());
            after_status = now + interval;
        }
        // A failed send desynchronised the stream; do not select on it or send
        // another frame, just return so the service loop reconnects.
        if (net_transport_broken()) return;

        int tcp = net_transport_tcp_fd();
        int wake_fd = net_transport_wake_fd();
        fd_set rfds;
        FD_ZERO(&rfds);
        FD_SET(tcp, &rfds);
        int maxfd = tcp;
        if (wake_fd >= 0) {
            FD_SET(wake_fd, &rfds);
            if (wake_fd > maxfd) maxfd = wake_fd;
        }
        int64_t rem_ms = after_status - now;
        if (rem_ms < 1) rem_ms = 1;
        if (rem_ms > interval) rem_ms = interval;
        if (ack_waiting && rem_ms > ACK_RETRY_MS) rem_ms = ACK_RETRY_MS;
        // Without the wake socket, inputs only arrive via this timeout.
        if (wake_fd < 0 && rem_ms > WAKE_POLL_MS) rem_ms = WAKE_POLL_MS;
        struct timeval tv = { .tv_sec = (time_t)(rem_ms / 1000), .tv_usec = (suseconds_t)((rem_ms % 1000) * 1000) };
        int ready = lwip_select(maxfd + 1, &rfds, NULL, NULL, &tv);
        if (ready < 0) {
            atomic_store_explicit(&telemetry_io.last_net_errno, errno, memory_order_relaxed);
            return;
        }
        if (!FD_ISSET(tcp, &rfds)) continue;

        uint8_t hbuf[LARK_PROTO_HDR_SIZE];
        if (!net_transport_recv_exact(hbuf, sizeof(hbuf), &wifi_online)) return;
        lark_hdr_t hdr;
        if (!lark_unpack_hdr(hbuf, &hdr)) {
            ESP_LOGW(TAG, "Invalid TCP frame header");
            return;
        }
        if (hdr.length <= FRAME_PACKET_CAP) {
            if (hdr.length && !net_transport_recv_exact(rx_buffer, hdr.length, &wifi_online)) return;
            handle_tcp_message(session, hdr.type, rx_buffer, hdr.length);
        } else {
            // Drain oversized non-frame payloads cleanly in chunks.
            uint32_t rem = hdr.length;
            while (rem > 0) {
                uint32_t chunk = rem > FRAME_PACKET_CAP ? FRAME_PACKET_CAP : rem;
                if (!net_transport_recv_exact(rx_buffer, chunk, &wifi_online)) return;
                if (hdr.type == LARK_MSG_TTS_PCM) receive_tts_pcm(rx_buffer, chunk);
                rem -= chunk;
            }
        }
    }
}

static void service_task(void *arg)
{
    (void)arg;
    net_backoff_init(&backoff);
    for (;;) {
        if (!atomic_load(&wifi_online)) {
            close_sockets();
            while (!atomic_load(&wifi_online)) {
                ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
            }
            continue;
        }
        net_transport_init_wake();
        uint32_t session = 0;
        if (!open_connection(&session)) {
            close_sockets();
            int delay = net_backoff_delay_ms(&backoff);
            ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(delay));
            net_backoff_failed(&backoff);
            continue;
        }

        run_session(session);
        bool welcomed = ready_socket_session() == session;
        bool rejected = session_rejected;
        close_sockets();
        if (welcomed) {
            // WELCOME already reset the delay; retry a healthy session quickly.
            ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(500));
        } else {
            int delay = net_backoff_delay_ms(&backoff);
            ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(delay));
            // Rejects are bounded too: a bad token or version keeps retrying
            // at the 30 s cap so a server-side fix recovers without a reboot.
            if (rejected) net_backoff_rejected(&backoff);
            else net_backoff_failed(&backoff);
        }
    }
}

void net_client_network_changed(bool online)
{
    atomic_store(&wifi_online, online);
    wake_service();
}

esp_err_t net_client_init(void)
{
    if (!lark_parse_edge_url(CONFIG_LARK_URL, edge_host, sizeof(edge_host), &edge_port)) {
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
    ESP_LOGI(TAG, "Native Lark device client targeting %s:%u", edge_host, edge_port);
    return ESP_OK;
}
