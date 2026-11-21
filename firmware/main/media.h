#pragma once

#include "audio.h"

enum { MEDIA_TARGET_CAP = 64 };

esp_err_t media_init(bool audio_ready);
bool media_audio_ready(void);
void media_set_wake_word(bool enabled);
bool media_record_start(uint16_t rec_session, uint8_t target, bool bridge_endpoint);
void media_record_stop(bool cancelled);
bool media_is_recording(void);
bool media_tts_start(uint32_t audio_id);
void media_tts_end(void);
bool media_tts_pcm(const void *data, size_t bytes);
bool media_call_start(uint32_t id, bool muted);
void media_call_end(uint32_t id);
void media_call_set_mute(uint32_t id, bool muted);
bool media_call_active(void);
bool media_call_sending(uint32_t id);
void media_call_pcm(const void *data, size_t bytes);
void media_interrupt(void);
void media_link_lost(void);
void media_chime(audio_tone_t tone);
