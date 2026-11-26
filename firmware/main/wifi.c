#include "wifi.h"

#include <stdio.h>
#include <string.h>

#include "board.h"
#include "esp_app_desc.h"
#include "esp_check.h"
#include "esp_event.h"
#include "esp_mac.h"
#include "esp_netif_sntp.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "mdns.h"
#include "sdkconfig.h"
#include "telemetry.h"

static const char *TAG = "wifi";
static esp_netif_t *sta;
static esp_timer_handle_t retry_timer;
static wifi_state_cb_t state_cb;
static bool sntp_started;
static bool mdns_started;

char device_id[13];
char device_mac[18];

const char *wifi_character(void)
{
#ifdef LARK_BOX3
    return "BOX-3";
#else
    return "Lark";
#endif
}

esp_err_t wifi_init_identity(void)
{
    uint8_t device_mac_bytes[6];
    ESP_RETURN_ON_ERROR(esp_read_mac(device_mac_bytes, ESP_MAC_WIFI_STA), TAG, "Read STA MAC");
    snprintf(device_id, sizeof(device_id), BOARD_ID_PREFIX "%02x%02x", device_mac_bytes[4], device_mac_bytes[5]);
    snprintf(device_mac, sizeof(device_mac), MACSTR, MAC2STR(device_mac_bytes));
    return ESP_OK;
}

bool wifi_set_low_latency(bool enable)
{
    esp_err_t err = esp_wifi_set_ps(enable ? WIFI_PS_NONE : WIFI_PS_MIN_MODEM);
    if (err == ESP_OK) return true;
    if (err != ESP_ERR_WIFI_NOT_INIT && err != ESP_ERR_WIFI_NOT_STARTED) {
        ESP_LOGW(TAG, "Set Wi-Fi PS(%d): %s", enable ? WIFI_PS_NONE : WIFI_PS_MIN_MODEM, esp_err_to_name(err));
    }
    return false;
}

void wifi_get_ip(char ip[WIFI_IP_LEN])
{
    esp_netif_ip_info_t info;
    if (sta && esp_netif_is_netif_up(sta) && esp_netif_get_ip_info(sta, &info) == ESP_OK) {
        snprintf(ip, WIFI_IP_LEN, IPSTR, IP2STR(&info.ip));
    } else {
        strcpy(ip, "0.0.0.0");
    }
}

static void schedule_retry(void)
{
    if (!esp_timer_is_active(retry_timer)) {
        esp_timer_start_once(retry_timer, 2000000);
    }
}

static void connect(void *arg)
{
    esp_err_t err = esp_wifi_connect();
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Connect: %s", esp_err_to_name(err));
        schedule_retry();
    }
}

static esp_err_t start_mdns(void)
{
    char instance[40];
    snprintf(instance, sizeof(instance), "%s (%s)", wifi_character(), device_id);
    mdns_txt_item_t txt[] = {
        { "character", wifi_character() },
        { "firmware", esp_app_get_description()->version },
        { "mac", device_mac },
    };
    ESP_RETURN_ON_ERROR(mdns_init(), TAG, "Start mDNS");
    esp_err_t err = mdns_hostname_set(device_id);
    if (err == ESP_OK) {
        err = mdns_instance_name_set(instance);
    }
    if (err == ESP_OK) {
        err = mdns_service_add(instance, "_http", "_tcp", 80, txt, sizeof(txt) / sizeof(txt[0]));
    }
    if (err != ESP_OK) {
        mdns_free();
    }
    return err;
}

static void on_event(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
        connect(NULL);
    } else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
        const wifi_event_sta_disconnected_t *event = data;
        ESP_LOGW(TAG, "Disconnected (reason=%u); reconnecting", event->reason);
        atomic_fetch_add_explicit(&telemetry_io.wifi_disconnects, 1, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.last_wifi_reason, (int)event->reason, memory_order_relaxed);
        state_cb(false, NULL);
        schedule_retry();
    } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
        const ip_event_got_ip_t *event = data;
        char ip[WIFI_IP_LEN];
        snprintf(ip, sizeof(ip), IPSTR, IP2STR(&event->ip_info.ip));
        ESP_LOGI(TAG, "STA IP %s", ip);
        if (!sntp_started) {
            esp_err_t err = esp_netif_sntp_start();
            sntp_started = (err == ESP_OK);
            if (err != ESP_OK) {
                ESP_LOGW(TAG, "SNTP start: %s", esp_err_to_name(err));
            }
        }
        state_cb(true, ip);
        if (!mdns_started) {
            esp_err_t err = start_mdns();
            mdns_started = err == ESP_OK;
            if (err != ESP_OK) {
                ESP_LOGW(TAG, "mDNS unavailable: %s", esp_err_to_name(err));
            }
        }
    }
}

esp_err_t wifi_start(wifi_state_cb_t on_state)
{
    state_cb = on_state;
    ESP_RETURN_ON_ERROR(esp_netif_init(), TAG, "Init netif");
    ESP_RETURN_ON_ERROR(esp_event_loop_create_default(), TAG, "Create event loop");
    sta = esp_netif_create_default_wifi_sta();
    ESP_RETURN_ON_FALSE(sta, ESP_ERR_NO_MEM, TAG, "Create STA netif");
    ESP_RETURN_ON_ERROR(esp_netif_set_hostname(sta, device_id), TAG, "Set DHCP hostname");

    esp_sntp_config_t sntp = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
    sntp.start = false;
    ESP_RETURN_ON_ERROR(esp_netif_sntp_init(&sntp), TAG, "Init SNTP");
    const esp_timer_create_args_t timer = { .callback = connect, .name = "wifi_retry" };
    ESP_RETURN_ON_ERROR(esp_timer_create(&timer, &retry_timer), TAG, "Create retry timer");

    wifi_init_config_t init = WIFI_INIT_CONFIG_DEFAULT();
    ESP_RETURN_ON_ERROR(esp_wifi_init(&init), TAG, "Init Wi-Fi");
    ESP_RETURN_ON_ERROR(esp_wifi_set_storage(WIFI_STORAGE_RAM), TAG, "Select RAM config");
    ESP_RETURN_ON_ERROR(esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_event, NULL),
                        TAG, "Register Wi-Fi events");
    ESP_RETURN_ON_ERROR(esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_event, NULL),
                        TAG, "Register IP events");

    wifi_config_t config = { 0 };
    _Static_assert(sizeof(CONFIG_LARK_WIFI_SSID) - 1 <= sizeof(config.sta.ssid), "SSID too long");
    _Static_assert(sizeof(CONFIG_LARK_WIFI_PASSWORD) - 1 <= sizeof(config.sta.password),
                   "Wi-Fi password too long");
    memcpy(config.sta.ssid, CONFIG_LARK_WIFI_SSID, sizeof(CONFIG_LARK_WIFI_SSID) - 1);
    memcpy(config.sta.password, CONFIG_LARK_WIFI_PASSWORD, sizeof(CONFIG_LARK_WIFI_PASSWORD) - 1);
    config.sta.threshold.authmode = config.sta.password[0] ? WIFI_AUTH_WPA2_PSK : WIFI_AUTH_OPEN;
    ESP_RETURN_ON_ERROR(esp_wifi_set_mode(WIFI_MODE_STA), TAG, "Set STA mode");
    ESP_RETURN_ON_ERROR(esp_wifi_set_config(WIFI_IF_STA, &config), TAG, "Set STA config");
    return esp_wifi_start();
}
