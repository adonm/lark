#include "audio.h"
#include "display.h"
#include "media.h"
#include "ota_server.h"
#include "power.h"
#include "wifi.h"
#include "net_client.h"

#include "esp_log.h"
#include "esp_timer.h"
#include "nvs_flash.h"

static const char *TAG = "lark";

static void on_wifi(bool online, const char *ip)
{
    net_client_network_changed(online);
}

void app_main(void)
{
    int64_t boot_us = esp_timer_get_time();
    esp_err_t nvs = nvs_flash_init();
    if (nvs == ESP_ERR_NVS_NO_FREE_PAGES || nvs == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        nvs = nvs_flash_init();
    }
    ESP_ERROR_CHECK(nvs);
    ESP_ERROR_CHECK(wifi_init_identity());
    ESP_ERROR_CHECK(media_init(audio_init() == ESP_OK));
    ESP_ERROR_CHECK(display_init());
    esp_err_t net = net_client_init();
    if (net != ESP_OK) ESP_LOGW(TAG, "Bridge client unavailable: %s", esp_err_to_name(net));
    if (power_start() != ESP_OK) ESP_LOGW(TAG, "Battery monitor unavailable");
    ESP_ERROR_CHECK(wifi_start(on_wifi));
    ESP_ERROR_CHECK(ota_server_start());
    ota_check_boot(boot_us);
    ESP_LOGI(TAG, "%s ready", device_id);
}
