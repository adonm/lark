#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "esp_err.h"

#define WIFI_IP_LEN 16

typedef void (*wifi_state_cb_t)(bool connected, const char *ip);

extern char device_id[13];
extern char device_mac[18];

esp_err_t wifi_init_identity(void);
const char *wifi_character(void);
esp_err_t wifi_start(wifi_state_cb_t on_state);
void wifi_get_ip(char ip[WIFI_IP_LEN]);
void wifi_set_low_latency(bool enable);
