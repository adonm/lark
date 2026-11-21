#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

esp_err_t display_init(void);
void display_set_link(bool connected);
bool display_apply_frame(const uint8_t *data, size_t len);
void display_wake(void);
void display_set_brightness(int level);
bool display_is_sleeping(void);
void display_sync_power_save(void);
