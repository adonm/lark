#include "net_transport.h"

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>

#include "lwip/netdb.h"
#include "proto.h"
#include "telemetry.h"
#include "wake_gate.h"

enum {
    TCP_BUF_BYTES = 64 * 1024,
    UDP_BUF_BYTES = 16 * 1024,
    CONNECT_TIMEOUT_MS = 5000,
    // Above the Wi-Fi PS wake budget, so a modem-sleep wake never turns a
    // healthy control frame into a false session failure.
    SEND_TIMEOUT_MS = 2000,
    RECV_TIMEOUT_MS = 5000,
    TCP_KEEP_IDLE_S = 15,
    TCP_KEEP_INTVL_S = 5,
    TCP_KEEP_CNT = 3,
    // Every client-to-server payload is a control message; frames only travel
    // the other way. One stack buffer makes header+payload a single send.
    TX_FRAME_CAP = 1024,
};

static portMUX_TYPE udp_lock = portMUX_INITIALIZER_UNLOCKED;
static int tcp_fd = -1;
static int udp_fd = -1;
static int wake_rx_fd = -1;
static int wake_tx_fd = -1;
static wake_gate_t wake_gate;
static struct sockaddr_in bridge_udp_addr;
static uint32_t active_ssrc;
static uint16_t rtp_seq;
static uint32_t rtp_ts;
static _Alignas(4) uint8_t rtp_pkt[LARK_RTP_PACKET_BYTES];

static uint16_t tx_seq;
static atomic_bool session_broken;

static void note_net_errno(int err)
{
    if (err) atomic_store_explicit(&telemetry_io.last_net_errno, err, memory_order_relaxed);
}

bool net_transport_resolve(const char *host, uint16_t port, struct sockaddr_in *remote)
{
    char port_str[8];
    snprintf(port_str, sizeof(port_str), "%u", port);
    struct addrinfo hints = { .ai_family = AF_INET, .ai_socktype = SOCK_STREAM };
    struct addrinfo *res = NULL;
    if (lwip_getaddrinfo(host, port_str, &hints, &res) != 0 || !res) return false;
    *remote = *(const struct sockaddr_in *)res->ai_addr;
    lwip_freeaddrinfo(res);
    return true;
}

// A failed or short send leaves the stream desynchronised. Never try another
// frame on it: mark it broken so the session closes and reconnects at once.
static bool send_all_tcp(int fd, const void *data, size_t len)
{
    const uint8_t *p = data;
    size_t sent = 0;
    while (sent < len) {
        int n = lwip_send(fd, p + sent, len - sent, 0);
        if (n <= 0) {
            note_net_errno(errno);
            atomic_store_explicit(&session_broken, true, memory_order_relaxed);
            return false;
        }
        sent += (size_t)n;
    }
    return true;
}

bool net_transport_send(uint8_t type, const void *payload, uint32_t len)
{
    if (tcp_fd < 0 || len > LARK_PROTO_MAX_PAYLOAD) return false;
    if (atomic_load_explicit(&session_broken, memory_order_relaxed)) return false;
    if (len > TX_FRAME_CAP) {
        note_net_errno(EMSGSIZE);
        atomic_store_explicit(&session_broken, true, memory_order_relaxed);
        return false;
    }

    uint8_t buf[LARK_PROTO_HDR_SIZE + TX_FRAME_CAP];
    lark_pack_hdr(buf, type, tx_seq++, len);
    if (len) memcpy(buf + LARK_PROTO_HDR_SIZE, payload, len);
    return send_all_tcp(tcp_fd, buf, LARK_PROTO_HDR_SIZE + len);
}

bool net_transport_recv_exact(uint8_t *dst, size_t len, const atomic_bool *online)
{
    size_t got = 0;
    while (got < len) {
        if (!atomic_load(online)) return false;
        int n = lwip_recv(tcp_fd, dst + got, len - got, 0);
        if (n > 0) {
            got += (size_t)n;
            continue;
        }
        if (n == 0) {
            // Peer closed mid-frame; report it as a reset for /status.
            note_net_errno(ECONNRESET);
            return false;
        }
        if (errno == EINTR) continue;
        // SO_RCVTIMEO bounds every recv, so EAGAIN means the 5 s mid-frame
        // stall guard fired rather than a transient scheduling hiccup.
        note_net_errno(errno);
        return false;
    }
    return true;
}

static void open_wake_sockets(void)
{
    if (wake_rx_fd >= 0) return;
    int rfd = lwip_socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    int tfd = lwip_socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (rfd < 0 || tfd < 0) {
        if (rfd >= 0) lwip_close(rfd);
        if (tfd >= 0) lwip_close(tfd);
        return;
    }
    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_addr.s_addr = htonl(INADDR_LOOPBACK),
        .sin_port = 0,
    };
    socklen_t alen = sizeof(addr);
    if (lwip_bind(rfd, (struct sockaddr *)&addr, sizeof(addr)) != 0 ||
        lwip_getsockname(rfd, (struct sockaddr *)&addr, &alen) != 0 ||
        lwip_connect(tfd, (const struct sockaddr *)&addr, sizeof(addr)) != 0) {
        lwip_close(rfd);
        lwip_close(tfd);
        return;
    }
    lwip_fcntl(rfd, F_SETFL, lwip_fcntl(rfd, F_GETFL, 0) | O_NONBLOCK);
    lwip_fcntl(tfd, F_SETFL, lwip_fcntl(tfd, F_GETFL, 0) | O_NONBLOCK);
    wake_rx_fd = rfd;
    wake_tx_fd = tfd;
}

void net_transport_kick(void)
{
    if (wake_tx_fd >= 0 && wake_gate_request(&wake_gate)) {
        uint8_t b = 1;
        if (lwip_send(wake_tx_fd, &b, sizeof(b), 0) <= 0) {
            // Retry on the next request instead of coalescing into a lost datagram.
            wake_gate_clear(&wake_gate);
        }
    }
}

static int wake_recv(void *ctx, void *buf, size_t len)
{
    return lwip_recv((int)(intptr_t)ctx, buf, len, 0);
}

void net_transport_drain_wake(void)
{
    if (wake_rx_fd < 0) {
        wake_gate_clear(&wake_gate);
        return;
    }
    wake_gate_drain(&wake_gate, wake_recv, (void *)(intptr_t)wake_rx_fd);
}

static int open_tcp_socket(const struct sockaddr_in *remote)
{
    int fd = lwip_socket(AF_INET, SOCK_STREAM, IPPROTO_TCP);
    if (fd < 0) return -1;
    int one = 1;
    int buf = TCP_BUF_BYTES;
    int keep_idle = TCP_KEEP_IDLE_S;
    int keep_intvl = TCP_KEEP_INTVL_S;
    int keep_cnt = TCP_KEEP_CNT;
    lwip_setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));
    lwip_setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof(one));
    lwip_setsockopt(fd, IPPROTO_TCP, TCP_KEEPIDLE, &keep_idle, sizeof(keep_idle));
    lwip_setsockopt(fd, IPPROTO_TCP, TCP_KEEPINTVL, &keep_intvl, sizeof(keep_intvl));
    lwip_setsockopt(fd, IPPROTO_TCP, TCP_KEEPCNT, &keep_cnt, sizeof(keep_cnt));
    lwip_setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &buf, sizeof(buf));
    lwip_setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &buf, sizeof(buf));

    int flags = lwip_fcntl(fd, F_GETFL, 0);
    lwip_fcntl(fd, F_SETFL, flags | O_NONBLOCK);
    int rc = lwip_connect(fd, (const struct sockaddr *)remote, sizeof(*remote));
    if (rc < 0 && errno != EINPROGRESS) {
        lwip_close(fd);
        return -1;
    }
    if (rc < 0) {
        fd_set wfds;
        FD_ZERO(&wfds);
        FD_SET(fd, &wfds);
        struct timeval tv = { .tv_sec = CONNECT_TIMEOUT_MS / 1000, .tv_usec = 0 };
        int err = 0;
        socklen_t elen = sizeof(err);
        if (lwip_select(fd + 1, NULL, &wfds, NULL, &tv) <= 0 ||
            lwip_getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &elen) != 0 || err != 0) {
            lwip_close(fd);
            return -1;
        }
    }
    lwip_fcntl(fd, F_SETFL, flags);
    struct timeval snd_tv = {
        .tv_sec = SEND_TIMEOUT_MS / 1000,
        .tv_usec = (SEND_TIMEOUT_MS % 1000) * 1000,
    };
    struct timeval rcv_tv = {
        .tv_sec = RECV_TIMEOUT_MS / 1000,
        .tv_usec = (RECV_TIMEOUT_MS % 1000) * 1000,
    };
    lwip_setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &snd_tv, sizeof(snd_tv));
    // Without this, recv_exact blocks forever on a half-open socket and its
    // 5 s mid-frame stall guard never fires.
    lwip_setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &rcv_tv, sizeof(rcv_tv));
    return fd;
}

static int open_udp_socket(uint16_t *local_port)
{
    int fd = lwip_socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (fd < 0) return -1;
    int buf = UDP_BUF_BYTES;
    lwip_setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &buf, sizeof(buf));
    struct sockaddr_in bind_addr = { .sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_ANY), .sin_port = 0 };
    socklen_t blen = sizeof(bind_addr);
    if (lwip_bind(fd, (struct sockaddr *)&bind_addr, sizeof(bind_addr)) != 0 ||
        lwip_getsockname(fd, (struct sockaddr *)&bind_addr, &blen) != 0) {
        lwip_close(fd);
        return -1;
    }
    lwip_fcntl(fd, F_SETFL, lwip_fcntl(fd, F_GETFL, 0) | O_NONBLOCK);
    *local_port = ntohs(bind_addr.sin_port);
    return fd;
}

bool net_transport_open(const struct sockaddr_in *remote, uint16_t *local_udp_port)
{
    int tfd = open_tcp_socket(remote);
    uint16_t local_port = 0;
    int ufd = tfd >= 0 ? open_udp_socket(&local_port) : -1;
    if (tfd < 0 || ufd < 0) {
        note_net_errno(errno);
        if (tfd >= 0) lwip_close(tfd);
        if (ufd >= 0) lwip_close(ufd);
        return false;
    }

    tcp_fd = tfd;
    tx_seq = 0;
    atomic_store_explicit(&session_broken, false, memory_order_relaxed);

    portENTER_CRITICAL(&udp_lock);
    udp_fd = ufd;
    bridge_udp_addr = *remote;
    bridge_udp_addr.sin_port = 0;
    active_ssrc = 0;
    portEXIT_CRITICAL(&udp_lock);

    *local_udp_port = local_port;
    return true;
}

void net_transport_close(void)
{
    atomic_store_explicit(&session_broken, false, memory_order_relaxed);
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
}

int net_transport_tcp_fd(void)
{
    return tcp_fd;
}

int net_transport_wake_fd(void)
{
    return wake_rx_fd;
}

bool net_transport_broken(void)
{
    return atomic_load_explicit(&session_broken, memory_order_relaxed);
}

void net_transport_clear_broken(void)
{
    atomic_store_explicit(&session_broken, false, memory_order_relaxed);
}

void net_transport_init_wake(void)
{
    open_wake_sockets();
}

void net_transport_set_udp_peer(uint16_t port, uint32_t ssrc)
{
    portENTER_CRITICAL(&udp_lock);
    bridge_udp_addr.sin_port = htons(port);
    active_ssrc = ssrc;
    rtp_seq = 0;
    rtp_ts = 0;
    portEXIT_CRITICAL(&udp_lock);
}

void net_transport_clear_udp_peer(void)
{
    portENTER_CRITICAL(&udp_lock);
    active_ssrc = 0;
    bridge_udp_addr.sin_port = 0;
    portEXIT_CRITICAL(&udp_lock);
}

int16_t *net_transport_rtp_buf(void)
{
    return (int16_t *)(rtp_pkt + LARK_RTP_HDR_SIZE);
}

bool net_transport_send_rtp(uint8_t pt, const int16_t *pcm, size_t samples)
{
    int fd = -1;
    struct sockaddr_in dst;
    uint32_t ssrc = 0, ts = 0;
    uint16_t seq = 0;

    portENTER_CRITICAL(&udp_lock);
    if (udp_fd >= 0 && active_ssrc != 0 && bridge_udp_addr.sin_port != 0) {
        fd = udp_fd;
        dst = bridge_udp_addr;
        ssrc = active_ssrc;
        seq = rtp_seq++;
        ts = rtp_ts;
        rtp_ts += (uint32_t)samples;
    }
    portEXIT_CRITICAL(&udp_lock);

    if (fd < 0) return false;
    size_t bytes = samples * sizeof(int16_t);
    lark_rtp_encode(rtp_pkt, pt, seq, ts, ssrc);
    if (pcm != (const int16_t *)(rtp_pkt + LARK_RTP_HDR_SIZE)) {
        memcpy(rtp_pkt + LARK_RTP_HDR_SIZE, pcm, bytes);
    }
    int sent = lwip_sendto(fd, rtp_pkt, LARK_RTP_HDR_SIZE + bytes, 0, (const struct sockaddr *)&dst, sizeof(dst));
    return sent == (int)(LARK_RTP_HDR_SIZE + bytes);
}
