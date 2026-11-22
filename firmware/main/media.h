#pragma once

#include "audio.h"

enum { MEDIA_TARGET_CAP = 64, DEBUG_MIC_MAX_BYTES = 16000 * 10 * sizeof(int16_t) };

esp_err_t media_init(bool audio_ready);
bool media_audio_ready(void);
bool media_record_start(uint16_t rec_session, uint8_t target, bool bridge_endpoint, bool listen);
void media_record_stop(bool cancelled);
bool media_is_recording(void);
bool media_tts_start(uint32_t audio_id);
void media_tts_end(void);
bool media_tts_pcm(const void *data, size_t bytes);
void media_interrupt(void);
void media_link_lost(void);
void media_chime(audio_tone_t tone);
bool media_debug_mic_load(size_t offset, const uint8_t *data, size_t len, size_t total_len, bool commit);
bool media_debug_mic_read_10ms(int16_t *dst);
bool media_debug_mic_active(void);
