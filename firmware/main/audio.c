#include "audio.h"
#include "vad.h"

#include <math.h>
#include <stdatomic.h>
#include <string.h>

#include "bsp/esp-bsp.h"
#include "esp_check.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "nvs.h"

enum {
    CHANNELS = 1, FRAMES = AUDIO_10MS, MAX_FRAMES = AUDIO_RATE * AUDIO_MAX_SECONDS,
    LISTEN_MAX_FRAMES = AUDIO_RATE * AUDIO_LISTEN_MAX_SECONDS,
    FADE = AUDIO_RATE / 200, DMA_TAIL = 6 * 240, CHIME_FRAMES = AUDIO_RATE / 10,
};

static const char *TAG = "audio";
static esp_codec_dev_handle_t mic;
static esp_codec_dev_handle_t speaker;
static atomic_bool stop_playback;
static atomic_bool chime_requested;
static atomic_int volume = 78;
static int saved_volume = -1;
static SemaphoreHandle_t volume_lock;
static size_t played_samples;
static int chime_pos;
static int16_t last_sample;

int audio_get_volume(void)
{
    return atomic_load(&volume);
}

static esp_err_t set_volume(int level, bool persist)
{
    if (level < 0 || level > 100 || !volume_lock) {
        return ESP_ERR_INVALID_ARG;
    }
    xSemaphoreTake(volume_lock, portMAX_DELAY);
    esp_err_t err = ESP_OK;
    if (persist && level != saved_volume) {
        nvs_handle_t nvs;
        err = nvs_open("lark", NVS_READWRITE, &nvs);
        if (err == ESP_OK) {
            err = nvs_set_u8(nvs, "volume", level);
            if (err == ESP_OK) {
                err = nvs_commit(nvs);
            }
            nvs_close(nvs);
        }
        if (err == ESP_OK) {
            saved_volume = level;
        }
    }
    if (err == ESP_OK && level != audio_get_volume()) {
        err = esp_codec_dev_set_out_vol(speaker, level);
        if (err == ESP_OK) {
            atomic_store(&volume, level);
        }
    }
    xSemaphoreGive(volume_lock);
    return err;
}

esp_err_t audio_set_volume(int level)
{
    return set_volume(level, true);
}

esp_err_t audio_set_volume_live(int level)
{
    return set_volume(level, false);
}

static esp_err_t write_speaker(const int16_t *mono, size_t frames)
{
    return esp_codec_dev_write(speaker, (void *)mono, frames * sizeof(*mono));
}

esp_err_t audio_read_10ms(int16_t pcm[AUDIO_10MS])
{
    return esp_codec_dev_read(mic, pcm, FRAMES * sizeof(*pcm));
}

esp_err_t audio_drain_input(void)
{
    int16_t pcm[FRAMES];
    // Discard only stale frames already buffered in RX DMA without blocking for fresh ones.
    for (int i = 0; i < DMA_TAIL / FRAMES; ++i) {
        int64_t t0 = esp_timer_get_time();
        ESP_RETURN_ON_ERROR(audio_read_10ms(pcm), TAG, "Drain mic DMA");
        if (esp_timer_get_time() - t0 >= 3000) break;
    }
    return ESP_OK;
}

esp_err_t audio_write_10ms(const int16_t pcm[AUDIO_10MS])
{
    return write_speaker(pcm, FRAMES);
}

esp_err_t audio_init(void)
{
    const i2s_std_config_t i2s = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(AUDIO_RATE),
        .slot_cfg = I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_MONO),
        .gpio_cfg = {
            .mclk = BSP_I2S_MCLK, .bclk = BSP_I2S_SCLK, .ws = BSP_I2S_LCLK,
            .dout = BSP_I2S_DOUT, .din = BSP_I2S_DSIN,
        },
    };
    ESP_RETURN_ON_ERROR(bsp_i2c_init(), TAG, "Init codec I2C");
    ESP_RETURN_ON_ERROR(bsp_audio_init(&i2s), TAG, "Init duplex I2S");
    mic = bsp_audio_codec_microphone_init();
    speaker = bsp_audio_codec_speaker_init();
    esp_err_t ret;
    ESP_GOTO_ON_FALSE(mic && speaker, ESP_FAIL, fail, TAG, "Init mic and speaker");

    esp_codec_dev_sample_info_t format = {
        .sample_rate = AUDIO_RATE, .channel = CHANNELS, .bits_per_sample = 16,
    };
    volume_lock = xSemaphoreCreateMutex();
    ESP_GOTO_ON_FALSE(volume_lock, ESP_ERR_NO_MEM, fail, TAG, "Create volume lock");
    nvs_handle_t nvs;
    if (nvs_open("lark", NVS_READONLY, &nvs) == ESP_OK) {
        uint8_t saved;
        if (nvs_get_u8(nvs, "volume", &saved) == ESP_OK && saved <= 100) {
            saved_volume = saved;
            atomic_store(&volume, saved);
        }
        nvs_close(nvs);
    }
    ESP_GOTO_ON_ERROR(esp_codec_dev_set_out_vol(speaker, audio_get_volume()), fail, TAG, "Set speaker volume");
    ESP_GOTO_ON_ERROR(esp_codec_dev_open(speaker, &format), fail, TAG, "Open ES8311");
    ESP_GOTO_ON_ERROR(esp_codec_dev_open(mic, &format), fail, TAG, "Open ES7210");
    int16_t probe[FRAMES];
    ESP_GOTO_ON_ERROR(esp_codec_dev_read(mic, probe, sizeof(probe)), fail, TAG, "Read initial I2S frames");

    ESP_LOGI(TAG, "ES8311 speaker and ES7210 microphone initialized (%d Hz mono Philips I2S)", AUDIO_RATE);
    return ESP_OK;

fail:
    esp_codec_dev_delete(mic);
    esp_codec_dev_delete(speaker);
    mic = speaker = NULL;
    if (volume_lock) {
        vSemaphoreDelete(volume_lock);
        volume_lock = NULL;
    }
    return ret;
}

static int16_t tone_sample(int n, int length, float hz, float amplitude)
{
    int edge = n < length - 1 - n ? n : length - 1 - n;
    float gain = edge <= 0 ? 0.0f : edge < FADE ? (float)edge / FADE : 1.0f;
    return (int16_t)(amplitude * gain * sinf(n * (2.0f * (float)M_PI / AUDIO_RATE) * hz));
}

static int16_t chirp(int n)
{
    const int beat = CHIME_FRAMES / 2;
    return tone_sample(n % beat, beat, n < beat ? 659.25f : 880.0f, 4400.0f);
}

void audio_tone(audio_tone_t tone, bool (*stop_requested)(const void *), const void *ctx)
{
    const int beat = AUDIO_RATE * (tone == AUDIO_RING_TONE ? 70 : 40) / 1000;
    const int length = tone == AUDIO_HAPPY_TONE ? CHIME_FRAMES : tone == AUDIO_START_TONE ? beat : 2 * beat;
    int16_t samples[FRAMES];

    // The BSP queues up to 6 × 240 frames (90 ms); send silence to play the tone
    // out before starting capture or waiting on the bridge.
    for (int offset = 0; offset < length + DMA_TAIL; offset += FRAMES) {
        if (stop_requested && stop_requested(ctx)) return;
        for (int i = 0; i < FRAMES; ++i) {
            int n = offset + i;
            float hz = tone == AUDIO_START_TONE ? 740.0f :
                       tone == AUDIO_THINK_TONE ? (n < beat ? 370.0f : 493.9f) :
                       tone == AUDIO_RING_TONE ? (n < beat ? 660.0f : 880.0f) :
                       (n < beat ? 440.0f : 293.7f);
            samples[i] = n >= length ? 0 : tone == AUDIO_HAPPY_TONE ? chirp(n) :
                         tone_sample(n % beat, beat, hz, tone == AUDIO_QUIET_TONE ? 2700.0f : 4700.0f);
        }
        if (write_speaker(samples, FRAMES) != ESP_OK) {
            ESP_LOGW(TAG, "Could not play confirmation tone");
            return;
        }
    }
}

esp_err_t audio_record_stream(bool bridge_endpoint, bool listen, int16_t *chunk_buf, bool (*stop_requested)(void), audio_capture_sink_t sink, void *ctx)
{
    _Static_assert(AUDIO_RECORD_CHUNK_SAMPLES % FRAMES == 0, "Chunk must contain whole VAD frames");
    ESP_RETURN_ON_FALSE(chunk_buf, ESP_ERR_INVALID_ARG, TAG, "No record buffer");

    // Drain only already-buffered stale RX DMA frames (< 3 ms); keep the first fresh frame.
    bool have_first = false;
    for (int i = 0; i < 12; ++i) {
        int64_t t0 = esp_timer_get_time();
        ESP_RETURN_ON_ERROR(audio_read_10ms(chunk_buf), TAG, "Prepare recording");
        if (esp_timer_get_time() - t0 >= 3000) {
            have_first = true;
            break;
        }
    }

    size_t samples = 0, pending = 0;
    size_t max_frames = listen ? LISTEN_MAX_FRAMES : MAX_FRAMES;
    vad_t vad = { 0 };
    while (samples < max_frames && !stop_requested()) {
        int16_t *dst = chunk_buf + pending;
        if (have_first) {
            have_first = false;
        } else {
            ESP_RETURN_ON_ERROR(audio_read_10ms(dst), TAG, "Read mic");
        }
        uint64_t sum = 0;
        for (size_t i = 0; i < FRAMES; ++i) {
            int32_t sample = dst[i];
            sum += sample * sample;
        }
        pending += FRAMES;
        vad_result_t result = listen ? VAD_CONTINUE
                                     : bridge_endpoint ? (vad_bridge_watchdog(&vad, sum / FRAMES) ? VAD_SEND : VAD_CONTINUE)
                                                       : vad_update(&vad, sum / FRAMES);
        if (result == VAD_NO_SPEECH && !stop_requested()) {
            return ESP_ERR_NOT_FOUND;
        }
        samples += FRAMES;
        if (pending == AUDIO_RECORD_CHUNK_SAMPLES) {
            esp_err_t err = sink(chunk_buf, pending, ctx);
            if (err != ESP_OK) return err;
            pending = 0;
        }
        if (result != VAD_CONTINUE) {
            break;
        }
    }
    if (!samples) return ESP_ERR_NOT_FOUND;
    return pending ? sink(chunk_buf, pending, ctx) : ESP_OK;
}

void audio_stream_begin(void)
{
    atomic_store(&stop_playback, false);
    atomic_store(&chime_requested, false);
    played_samples = 0;
    chime_pos = CHIME_FRAMES;
    last_sample = 0;
}

void audio_stop_playback(void)
{
    atomic_store(&stop_playback, true);
}

void audio_request_chime(void)
{
    atomic_store(&chime_requested, true);
}

bool audio_playback_stopped(void)
{
    return atomic_load(&stop_playback);
}

esp_err_t audio_stream_write(const int16_t *pcm, size_t num_samples)
{
    ESP_RETURN_ON_FALSE(speaker && pcm && num_samples, ESP_ERR_INVALID_ARG, TAG, "No speech PCM");
    int16_t mono[FRAMES];
    size_t pos = 0;
    while (pos < num_samples && !audio_playback_stopped()) {
        if (atomic_exchange(&chime_requested, false)) {
            chime_pos = 0;
        }
        size_t count = num_samples - pos < FRAMES ? num_samples - pos : FRAMES;
        if (played_samples >= FADE && chime_pos >= CHIME_FRAMES) {
            last_sample = pcm[pos + count - 1];
            ESP_RETURN_ON_ERROR(write_speaker(pcm + pos, count), TAG, "Write speech PCM");
            pos += count;
            played_samples += count;
            continue;
        }
        for (size_t i = 0; i < count; ++i, ++pos, ++played_samples) {
            int32_t sample = pcm[pos];
            if (played_samples < FADE) {
                sample = sample * (int32_t)played_samples / FADE;
            }
            if (chime_pos < CHIME_FRAMES) {
                sample += chirp(chime_pos++);
                sample = sample > INT16_MAX ? INT16_MAX : sample < INT16_MIN ? INT16_MIN : sample;
            }
            last_sample = mono[i] = (int16_t)sample;
        }
        ESP_RETURN_ON_ERROR(write_speaker(mono, count), TAG, "Write speech PCM");
    }
    return pos == num_samples ? ESP_OK : ESP_ERR_INVALID_STATE;
}

esp_err_t audio_fade_out(int16_t last, bool (*stop_requested)(void))
{
    // Fade the last sample and drain the BSP's 90 ms TX queue before yielding the codec.
    int16_t mono[FRAMES] = { 0 };
    if (last && (!stop_requested || !stop_requested())) {
        for (int i = 0; i < FADE; ++i) {
            mono[i] = last * (FADE - 1 - i) / FADE;
        }
        ESP_RETURN_ON_ERROR(audio_write_10ms(mono), TAG, "Fade playback");
        memset(mono, 0, sizeof(mono));
    }
    for (int i = 0; i < DMA_TAIL && (!stop_requested || !stop_requested()); i += FRAMES) {
        ESP_RETURN_ON_ERROR(audio_write_10ms(mono), TAG, "Drain playback");
    }
    return ESP_OK;
}

esp_err_t audio_stream_end(void)
{
    return played_samples ? audio_fade_out(last_sample, audio_playback_stopped) : ESP_OK;
}
