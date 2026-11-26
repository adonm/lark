#include "ota_server.h"

#include <inttypes.h>
#include <stdio.h>
#include <string.h>

#include "audio.h"
#include "board.h"
#include "debug_parse.h"
#include "display.h"
#include "frame.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_http_server.h"
#include "esp_https_ota.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "media.h"
#include "power.h"
#include "sdkconfig.h"
#include "telemetry.h"
#include "telemetry_logic.h"
#include "wifi.h"
#include "net_client.h"
#include "proto.h"

static const char *TAG = "ota";

static bool pending_verification(void)
{
    esp_ota_img_states_t state;
    return esp_ota_get_state_partition(esp_ota_get_running_partition(), &state) == ESP_OK &&
           state == ESP_OTA_IMG_PENDING_VERIFY;
}

static esp_err_t text_response(httpd_req_t *request, const char *status, const char *message)
{
    httpd_resp_set_status(request, status);
    httpd_resp_set_type(request, "text/plain");
    return httpd_resp_sendstr(request, message);
}

static esp_err_t json_response(httpd_req_t *request, const char *json)
{
    httpd_resp_set_type(request, "application/json");
    return httpd_resp_sendstr(request, json);
}

static bool require_token(httpd_req_t *request)
{
    static const char expected[] = CONFIG_LARK_TOKEN;
    char actual[sizeof(expected)];
    size_t len = httpd_req_get_hdr_value_len(request, "X-Lark-Token");
    if (len && len == sizeof(expected) - 1 &&
        httpd_req_get_hdr_value_str(request, "X-Lark-Token", actual, sizeof(actual)) == ESP_OK) {
        unsigned diff = 0;
        for (size_t i = 0; i < len; ++i) {
            diff |= (unsigned char)expected[i] ^ (unsigned char)actual[i];
        }
        if (!diff) {
            return true;
        }
    }
    text_response(request, "401 Unauthorized", "Invalid device token\n");
    return false;
}

static size_t body_length(httpd_req_t *request, size_t max_len)
{
    size_t len = request->content_len;
    if (!len) {
        text_response(request, "400 Bad Request", "Expected a request body\n");
    } else if (len > max_len) {
        text_response(request, "413 Content Too Large", "Request body exceeds limit\n");
    } else {
        return len;
    }
    return 0;
}

static esp_err_t read_bytes(httpd_req_t *request, char *buf, size_t len)
{
    for (size_t offset = 0; offset < len;) {
        int count = httpd_req_recv(request, buf + offset, len - offset);
        if (count <= 0) {
            text_response(request, "408 Request Timeout", "Incomplete request body\n");
            return ESP_FAIL;
        }
        offset += count;
    }
    return ESP_OK;
}

static esp_err_t read_req_body(httpd_req_t *request, char *buf, size_t max_len, size_t *out_len)
{
    *out_len = body_length(request, max_len);
    return *out_len ? read_bytes(request, buf, *out_len) : ESP_ERR_INVALID_SIZE;
}

static esp_err_t status_get(httpd_req_t *request)
{
    char ip[WIFI_IP_LEN];
    wifi_get_ip(ip);
    telemetry_t hw = telemetry_read();
    const esp_partition_t *running = esp_ota_get_running_partition();
    battery_status_t battery = battery_status();
    char pct[12] = "null", mv[16] = "null";
    if (battery.pct >= 0) {
        snprintf(pct, sizeof(pct), "%d", battery.pct);
        snprintf(mv, sizeof(mv), "%u", battery.millivolts);
    }
    char json[1024];
    int len = snprintf(json, sizeof(json),
             "{\"device\":\"%s\",\"character\":\"%s\",\"mac\":\"%s\",\"ip\":\"%s\",\"wifi_rssi\":%d,"
             "\"free_heap\":%zu,\"min_free_heap\":%zu,\"free_psram\":%zu,\"uptime_s\":%" PRIu64 ",\"cpu_pct\":%u,"
             "\"firmware\":\"%s\",\"running_partition\":\"%s\",\"reset_reason\":\"%s\","
             "\"board\":\"" BOARD_NAME "\",\"width\":%d,\"height\":%d,\"has_battery\":%s,"
             "\"volume\":%d,\"battery_pct\":%s,\"battery_mv\":%s,\"charging\":%s,\"sleeping\":%s,\"connected\":%s,"
             "\"ota_pending\":%s"
             TELEMETRY_IO_JSON "}",
             device_id, wifi_character(), device_mac, ip, hw.wifi_rssi,
             hw.free_heap, hw.min_free_heap, hw.free_psram, hw.uptime_s, hw.cpu_pct,
             esp_app_get_description()->version, running->label,
             telemetry_reset_reason((int)esp_reset_reason()),
             FRAME_WIDTH, FRAME_HEIGHT, BOARD_HAS_BATTERY ? "true" : "false",
             audio_get_volume(), pct, mv, BOARD_HAS_BATTERY ? (battery.charging ? "true" : "false") : "null",
             display_is_sleeping() ? "true" : "false", net_client_connected() ? "true" : "false",
             pending_verification() ? "true" : "false",
             TELEMETRY_IO_VALUES(hw));
    if (len < 0 || (size_t)len >= sizeof(json)) {
        return text_response(request, "500 Internal Server Error", "Status response too large\n");
    }
    return json_response(request, json);
}

static esp_err_t respond_and_reboot(httpd_req_t *request, const char *message)
{
    esp_err_t err = text_response(request, "200 OK", message);
    // Give TCP a chance to transmit the response before resetting the interface.
    vTaskDelay(pdMS_TO_TICKS(500));
    esp_restart();
    return err;
}

static esp_err_t reboot_post(httpd_req_t *request)
{
    return respond_and_reboot(request, "Rebooting\n");
}

static esp_err_t volume_post(httpd_req_t *request)
{
    char body[64];
    size_t bytes = request->content_len;
    if (!bytes || bytes >= sizeof(body)) {
        return text_response(request, "400 Bad Request", "Expected {\"level\":0..100}\n");
    }
    esp_err_t err = read_req_body(request, body, sizeof(body) - 1, &bytes);
    if (err != ESP_OK) {
        return err;
    }
    body[bytes] = '\0';
    int value = -1;
    if (!debug_parse_level(body, &value)) {
        return text_response(request, "400 Bad Request", "Expected an integer level from 0 to 100\n");
    }
    if (audio_set_volume(value) != ESP_OK) {
        return text_response(request, "503 Service Unavailable", "Could not save volume\n");
    }
    display_wake();
    media_chime(AUDIO_HAPPY_TONE);
    char reply[40];
    snprintf(reply, sizeof(reply), "{\"ok\":true,\"volume\":%d}", value);
    return json_response(request, reply);
}

static esp_err_t pull_headers(esp_http_client_handle_t client)
{
    return esp_http_client_set_header(client, "X-Lark-Token", CONFIG_LARK_TOKEN);
}

static bool matching_board(const esp_app_desc_t *image)
{
    const char *project = esp_app_get_description()->project_name;
    const char *incoming = image->project_name;
    return strncmp(incoming, project, sizeof(image->project_name)) == 0;
}

static esp_err_t ota_pull_post(httpd_req_t *request)
{
    char url[256];
    if (!strncmp(CONFIG_LARK_URL, "http://", 7) || !strncmp(CONFIG_LARK_URL, "https://", 8)) {
        int len = snprintf(url, sizeof(url), "%s%s", CONFIG_LARK_URL, BOARD_OTA_PATH);
        if (len < 0 || (size_t)len >= sizeof(url)) {
            return text_response(request, "500 Internal Server Error", "OTA URL too large\n");
        }
    } else {
        char host[128];
        uint16_t unused_port = 0;
        if (!lark_parse_bridge_url(CONFIG_LARK_URL, host, sizeof(host), &unused_port)) {
            return text_response(request, "500 Internal Server Error", "Invalid bridge URL\n");
        }
        const char *scheme = !strncmp(CONFIG_LARK_URL, "wss://", 6) ? "https" : "http";
        int len = strchr(host, ':') ? snprintf(url, sizeof(url), "%s://[%s]:%d%s", scheme, host, LARK_DEFAULT_HTTP_PORT, BOARD_OTA_PATH)
                                    : snprintf(url, sizeof(url), "%s://%s:%d%s", scheme, host, LARK_DEFAULT_HTTP_PORT, BOARD_OTA_PATH);
        if (len < 0 || (size_t)len >= sizeof(url)) {
            return text_response(request, "500 Internal Server Error", "OTA URL too large\n");
        }
    }
    const esp_http_client_config_t http = {
        .url = url,
        .crt_bundle_attach = esp_crt_bundle_attach,
        .timeout_ms = 15000,
        .disable_auto_redirect = true,
    };
    const esp_https_ota_config_t config = {
        .http_config = &http, .http_client_init_cb = pull_headers,
    };
    esp_https_ota_handle_t update = NULL;
    esp_app_desc_t image;
    esp_err_t err = esp_https_ota_begin(&config, &update);
    if (err == ESP_OK) err = esp_https_ota_get_img_desc(update, &image);
    if (err == ESP_OK && !matching_board(&image)) {
        esp_https_ota_abort(update);
        return text_response(request, "400 Bad Request", "Firmware is for a different board\n");
    }
    if (err == ESP_OK) {
        do {
            err = esp_https_ota_perform(update);
        } while (err == ESP_ERR_HTTPS_OTA_IN_PROGRESS);
    }
    if (err == ESP_OK && !esp_https_ota_is_complete_data_received(update)) err = ESP_ERR_INVALID_SIZE;
    if (err == ESP_OK) err = esp_https_ota_finish(update);
    else if (update) esp_https_ota_abort(update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Pull failed: %s", esp_err_to_name(err));
        return text_response(request, "502 Bad Gateway", "OTA pull failed\n");
    }
    return respond_and_reboot(request, "OTA installed; rebooting\n");
}

static esp_err_t ota_upload_post(httpd_req_t *request)
{
    const esp_partition_t *target = esp_ota_get_next_update_partition(NULL);
    if (!target) {
        return text_response(request, "500 Internal Server Error", "No OTA partition\n");
    }
    if (!request->content_len || request->content_len > target->size) {
        return text_response(request, "400 Bad Request", "Invalid image size\n");
    }
    esp_ota_handle_t update;
    esp_err_t err = esp_ota_begin(target, request->content_len, &update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Begin upload: %s", esp_err_to_name(err));
        return text_response(request, "500 Internal Server Error", "Could not start OTA\n");
    }

    char chunk[2048];
    size_t remaining = request->content_len;
    while (remaining > 0) {
        size_t len = remaining < sizeof(chunk) ? remaining : sizeof(chunk);
        if (read_bytes(request, chunk, len) != ESP_OK) {
            esp_ota_abort(update);
            return ESP_FAIL;
        }
        err = esp_ota_write(update, chunk, len);
        if (err != ESP_OK) {
            esp_ota_abort(update);
            ESP_LOGE(TAG, "Write upload: %s", esp_err_to_name(err));
            text_response(request, "400 Bad Request", "OTA write failed or image is invalid\n");
            return ESP_FAIL;
        }
        remaining -= len;
    }
    err = esp_ota_end(update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Verify upload: %s", esp_err_to_name(err));
        return text_response(request, "400 Bad Request", "Invalid firmware image\n");
    }
    esp_app_desc_t image;
    if (esp_ota_get_partition_description(target, &image) != ESP_OK || !matching_board(&image)) {
        return text_response(request, "400 Bad Request", "Firmware is for a different board\n");
    }
    err = esp_ota_set_boot_partition(target);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Set boot partition: %s", esp_err_to_name(err));
        return text_response(request, "500 Internal Server Error", "Could not activate OTA image\n");
    }
    return respond_and_reboot(request, "OTA installed; rebooting\n");
}

static esp_err_t debug_tap_post(httpd_req_t *request)
{
    char body[64];
    size_t bytes = 0;
    if (request->content_len >= sizeof(body)) {
        return text_response(request, "400 Bad Request", "Expected {\"x\":N,\"y\":M}\n");
    }
    esp_err_t err = read_req_body(request, body, sizeof(body) - 1, &bytes);
    if (err != ESP_OK) return err;
    body[bytes] = '\0';
    uint16_t x = 0, y = 0;
    if (!debug_parse_tap(body, FRAME_WIDTH, FRAME_HEIGHT, &x, &y)) {
        return text_response(request, "400 Bad Request", "Expected {\"x\":0..W-1,\"y\":0..H-1}\n");
    }
    if (!display_inject_tap(x, y)) {
        return text_response(request, "503 Service Unavailable", "Touch task unavailable\n");
    }
    char reply[48];
    snprintf(reply, sizeof(reply), "{\"ok\":true,\"x\":%u,\"y\":%u}\n", x, y);
    return json_response(request, reply);
}

static esp_err_t debug_mic_post(httpd_req_t *request)
{
    size_t total = request->content_len;
    // Invalidate any previously committed buffer even when this body is
    // rejected, so a stale sample never plays after a failed upload.
    uint32_t gen = media_debug_mic_begin();
    if (!debug_mic_len_valid(total, DEBUG_MIC_MAX_BYTES)) {
        return text_response(request, "400 Bad Request", "Expected 1..320000 even bytes of 16 kHz mono PCM\n");
    }
    uint8_t chunk[2048];
    size_t offset = 0;
    while (offset < total) {
        size_t want = total - offset < sizeof(chunk) ? total - offset : sizeof(chunk);
        int got = httpd_req_recv(request, (char *)chunk, want);
        if (got <= 0) {
            media_debug_mic_abort(gen);
            return text_response(request, "408 Request Timeout", "Incomplete PCM body\n");
        }
        bool last = offset + (size_t)got == total;
        if (!media_debug_mic_load(gen, offset, chunk, (size_t)got, total, last)) {
            media_debug_mic_abort(gen);
            return text_response(request, "503 Service Unavailable", "Debug mic buffer unavailable\n");
        }
        offset += (size_t)got;
    }
    char reply[48];
    snprintf(reply, sizeof(reply), "{\"ok\":true,\"bytes\":%zu}\n", total);
    return json_response(request, reply);
}

static esp_err_t debug_screen_get(httpd_req_t *request)
{
    if (!require_token(request)) return ESP_OK;
    char w_hdr[12], h_hdr[12];
    snprintf(w_hdr, sizeof(w_hdr), "%d", FRAME_WIDTH);
    snprintf(h_hdr, sizeof(h_hdr), "%d", FRAME_HEIGHT);
    httpd_resp_set_type(request, "application/octet-stream");
    httpd_resp_set_hdr(request, "X-Width", w_hdr);
    httpd_resp_set_hdr(request, "X-Height", h_hdr);

    enum { CHUNK_PIXELS = 2048 };
    uint16_t chunk[CHUNK_PIXELS];
    for (size_t off = 0; off < FRAME_PIXELS; off += CHUNK_PIXELS) {
        size_t count = FRAME_PIXELS - off < CHUNK_PIXELS ? FRAME_PIXELS - off : CHUNK_PIXELS;
        display_copy_fb_chunk(chunk, off, count);
        esp_err_t err = httpd_resp_send_chunk(request, (const char *)chunk, count * sizeof(uint16_t));
        if (err != ESP_OK) return err;
    }
    return httpd_resp_send_chunk(request, NULL, 0);
}

typedef struct {
    const char *uri;
    esp_err_t (*handler)(httpd_req_t *);
} post_route_t;

static esp_err_t checked_post(httpd_req_t *request)
{
    const post_route_t *route = request->user_ctx;
    return require_token(request) ? route->handler(request) : ESP_OK;
}

esp_err_t ota_server_start(void)
{
    static const post_route_t posts[] = {
        { "/ota", ota_pull_post }, { "/ota/upload", ota_upload_post }, { "/reboot", reboot_post },
        { "/volume", volume_post }, { "/debug/tap", debug_tap_post }, { "/debug/mic", debug_mic_post },
    };
    const httpd_uri_t status = { .uri = "/status", .method = HTTP_GET, .handler = status_get };
    const httpd_uri_t debug_screen = { .uri = "/debug/screen", .method = HTTP_GET, .handler = debug_screen_get };
    httpd_config_t config = HTTPD_DEFAULT_CONFIG();
    config.stack_size = 8192;
    config.max_uri_handlers = 2 + sizeof(posts) / sizeof(posts[0]);
    config.recv_wait_timeout = 15;
    config.lru_purge_enable = true;
    httpd_handle_t server;
    esp_err_t err = httpd_start(&server, &config);
    if (err != ESP_OK) {
        return err;
    }
    err = httpd_register_uri_handler(server, &status);
    if (err == ESP_OK) {
        err = httpd_register_uri_handler(server, &debug_screen);
    }
    for (size_t i = 0; err == ESP_OK && i < sizeof(posts) / sizeof(posts[0]); ++i) {
        const httpd_uri_t route = {
            .uri = posts[i].uri, .method = HTTP_POST, .handler = checked_post, .user_ctx = (void *)&posts[i],
        };
        err = httpd_register_uri_handler(server, &route);
    }
    if (err != ESP_OK) {
        httpd_stop(server);
    }
    return err;
}

void ota_check_boot(int64_t boot_us)
{
    if (!pending_verification()) return;
    // Roll back only if the bridge never completes HELLO_OK within 45 s of
    // boot. Audio init is logged but not fatal: a codec fault must not roll
    // back every image forever.
    if (!media_audio_ready()) {
        ESP_LOGE(TAG, "Audio unavailable at OTA health check; continuing without rollback");
    }
    int64_t remaining_us = 45 * 1000000LL - (esp_timer_get_time() - boot_us);
    if ((net_client_connected() || (remaining_us > 0 && net_client_wait_connected((uint32_t)(remaining_us / 1000))))) {
        esp_err_t err = esp_ota_mark_app_valid_cancel_rollback();
        if (err == ESP_OK) {
            ESP_LOGI(TAG, "Confirmed OTA image after bridge hello");
            return;
        }
        ESP_LOGE(TAG, "Confirm OTA image: %s", esp_err_to_name(err));
    }
    ESP_LOGE(TAG, "OTA boot health check failed (bridge=%d)", net_client_connected());
    esp_err_t err = esp_ota_mark_app_invalid_rollback_and_reboot();
    ESP_LOGE(TAG, "Roll back OTA image: %s", esp_err_to_name(err));
    // USB flashes may have no known-good OTA image. Keep local recovery reachable.
}
