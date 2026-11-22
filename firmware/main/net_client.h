#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

esp_err_t net_client_init(void);
void net_client_network_changed(bool online);
bool net_client_connected(void);
bool net_client_wait_connected(uint32_t timeout_ms);
uint32_t net_client_session(void);
void net_client_pointer(uint8_t kind, int x, int y, uint16_t rev_lo);
bool net_client_wake_word(void);
bool net_client_rec_start(uint32_t session, uint16_t rec_session, uint8_t target);
int16_t *net_client_rec_buf(void);
bool net_client_rec_pcm(uint32_t session, const int16_t *pcm, size_t samples);
void net_client_rec_end(uint32_t session, uint16_t rec_session, uint8_t target, bool cancelled);
void net_client_tts_ack(uint32_t session, uint32_t audio_id, bool aborted);
