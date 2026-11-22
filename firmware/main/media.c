#include "media.h"

#include <stdatomic.h>
#include <stdio.h>
#include <string.h>

#include "display.h"
#include "echo_suppress.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"
#include "telemetry.h"
#include "wake_word.h"
#include "net_client.h"

enum { TTS_BYTES = 1024 * 1024, TTS_BLOCK = 1280 };
typedef enum { RECORD, STREAM, CHIME, WAKE_CONTROL } job_kind_t;
typedef struct {
    job_kind_t kind;
    uint32_t session;
    unsigned chime_epoch;
    uint16_t rec_session;
    uint8_t target;
    audio_tone_t tone;
    bool bridge_endpoint;
    bool listen;
} job_t;

static QueueHandle_t jobs;
static SemaphoreHandle_t lock;
static TaskHandle_t media_task;
static StreamBufferHandle_t tts;
static StaticStreamBuffer_t tts_control;
// A queued STREAM job is a wakeup for the latest generation, not a second buffer owner.
static struct {
    uint32_t serial, session, audio_id;
    bool open, ended, queued;
} stream;
static atomic_bool recording, stop_recording, cancel_recording;
static atomic_bool wake_enabled;
static atomic_uint chime_epoch;
static atomic_bool tts_await_first;
static atomic_uint_fast64_t tts_first_byte_us;

bool media_audio_ready(void)
{
    return media_task != NULL;
}

static bool submit(const job_t *job)
{
    // Chimes must not displace recording, streaming or a call handover.
    if (job->kind == CHIME && uxQueueSpacesAvailable(jobs) <= 3) return false;
    return xQueueSend(jobs, job, 0) == pdPASS;
}

void media_set_wake_word(bool enabled)
{
    if (!jobs) return;
    if (atomic_exchange(&wake_enabled, enabled) != enabled) submit(&(job_t){ .kind = WAKE_CONTROL });
}

static void interrupt_locked(void)
{
    if (stream.open) {
        audio_stop_playback();
        stream.open = false;
        xTaskNotifyGive(media_task);
        net_client_tts_ack(stream.session, stream.audio_id, true);
    }
}

void media_interrupt(void)
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    interrupt_locked();
    xSemaphoreGive(lock);
}

void media_link_lost(void)
{
    media_set_wake_word(false);
    atomic_store(&cancel_recording, true);
    media_interrupt();
}

bool media_record_start(uint16_t rec_session, uint8_t target, bool bridge_endpoint, bool listen)
{
    if (!jobs) return false;
    job_t job = {
        .kind = RECORD,
        .session = net_client_session(),
        .rec_session = rec_session,
        .target = target,
        .bridge_endpoint = bridge_endpoint,
        .listen = listen,
    };
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = !recording && submit(&job);
    if (accepted) {
        recording = true;
        atomic_store(&cancel_recording, false);
        atomic_store(&stop_recording, false);
        interrupt_locked();
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_record_stop(bool cancelled)
{
    if (cancelled) atomic_store(&cancel_recording, true);
    atomic_store(&stop_recording, true);
}

bool media_is_recording(void)
{
    return atomic_load(&recording);
}

bool media_tts_start(uint32_t audio_id)
{
    if (!jobs) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    uint32_t session = net_client_session();
    bool accepted = session && !recording && !stream.open && xStreamBufferReset(tts) == pdPASS &&
                    (stream.queued || submit(&(job_t){ .kind = STREAM }));
    if (accepted) {
        ++stream.serial;
        stream.session = session;
        stream.audio_id = audio_id;
        stream.open = stream.queued = true;
        stream.ended = false;
        atomic_store_explicit(&tts_await_first, true, memory_order_relaxed);
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_tts_end(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    if (stream.open) {
        stream.ended = true;
        xTaskNotifyGive(media_task);
    }
    xSemaphoreGive(lock);
}

bool media_tts_pcm(const void *data, size_t bytes)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = false;
    if (stream.open && !stream.ended) {
        // The net_service task must not wait for speaker space; writes and reads
        // only hold this lock for the short buffer copies, never codec I/O.
        if (xStreamBufferSpacesAvailable(tts) >= bytes && xStreamBufferSend(tts, data, bytes, 0) == bytes) {
            if (atomic_exchange_explicit(&tts_await_first, false, memory_order_relaxed)) {
                atomic_store_explicit(&tts_first_byte_us, (uint64_t)esp_timer_get_time(), memory_order_relaxed);
            }
            accepted = true;
            xTaskNotifyGive(media_task);
        } else {
            atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
            interrupt_locked();
        }
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_chime(audio_tone_t tone)
{
    if (!jobs) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (!recording) {
        if (stream.open && tone == AUDIO_HAPPY_TONE) {
            audio_request_chime();
        } else {
            submit(&(job_t){ .kind = CHIME, .tone = tone, .chime_epoch = chime_epoch });
        }
    }
    xSemaphoreGive(lock);
}

static bool stop_capture(void)
{
    return atomic_load(&stop_recording) || atomic_load(&cancel_recording);
}

static bool capture_cancelled(const void *ctx)
{
    (void)ctx;
    return atomic_load(&cancel_recording);
}

static esp_err_t upload_pcm(const int16_t *pcm, size_t samples, void *ctx)
{
    const job_t *job = ctx;
    return !atomic_load(&cancel_recording) && net_client_rec_pcm(job->session, pcm, samples) ? ESP_OK : ESP_FAIL;
}

static void record(job_t *job)
{
    bool started = !atomic_load(&cancel_recording) && net_client_rec_start(job->session, job->rec_session, job->target);
    bool cancelled = true;
    if (started) {
        if (!job->listen) {
            audio_tone(AUDIO_START_TONE, capture_cancelled, NULL);
        }
        if (!capture_cancelled(NULL)) {
            esp_err_t err = audio_record_stream(job->bridge_endpoint, job->listen, net_client_rec_buf(),
                                                stop_capture, upload_pcm, job);
            cancelled = err != ESP_OK || capture_cancelled(NULL);
        }
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    recording = false;
    xSemaphoreGive(lock);
    if (started) net_client_rec_end(job->session, job->rec_session, job->target, cancelled);
}

static bool owns_stream(uint32_t serial)
{
    return stream.open && stream.serial == serial;
}

static void play(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    stream.queued = false;
    bool open = stream.open;
    uint32_t serial = stream.serial;
    if (open) audio_stream_begin();
    xSemaphoreGive(lock);
    if (!open) return;

    display_wake();
    int16_t chunk[TTS_BLOCK / sizeof(int16_t)];
    esp_err_t err = ESP_OK;
    bool played = false, starved = false;
    while (err == ESP_OK) {
        xSemaphoreTake(lock, portMAX_DELAY);
        bool owns = owns_stream(serial);
        // Keep receive/reset serialized; waits and codec I/O must not hold this lock.
        size_t bytes = owns ? xStreamBufferReceive(tts, chunk, sizeof(chunk), 0) : 0;
        bool done = !owns || (stream.ended && !bytes);
        xSemaphoreGive(lock);
        if (done) break;
        if (bytes % sizeof(*chunk)) {
            err = ESP_ERR_INVALID_SIZE;
        } else if (bytes) {
            if (!played) {
                uint64_t dt = (uint64_t)esp_timer_get_time() -
                              atomic_load_explicit(&tts_first_byte_us, memory_order_relaxed);
                atomic_store_explicit(&telemetry_io.tts_first_us, (unsigned)dt, memory_order_relaxed);
            }
            err = audio_stream_write(chunk, bytes / sizeof(*chunk));
            played = true;
            starved = false;
        } else {
            // Count starvation once per gap after playback has started, not
            // the initial prefill or the normal drain after an end frame.
            if (played && !starved) {
                atomic_fetch_add_explicit(&telemetry_io.audio_underruns, 1, memory_order_relaxed);
                starved = true;
            }
            ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
        }
    }
    esp_err_t end = audio_stream_end();
    xSemaphoreTake(lock, portMAX_DELAY);
    if (owns_stream(serial)) {
        stream.open = false;
        net_client_tts_ack(stream.session, stream.audio_id, err != ESP_OK || end != ESP_OK);
    }
    xSemaphoreGive(lock);
}

static bool chime_interrupted(const void *ctx)
{
    const job_t *job = ctx;
    return job->chime_epoch != chime_epoch;
}

static void worker(void *arg)
{
    (void)arg;
    bool detecting = false, drain_before_wake = true;
    int64_t wake_after_us = 0;
    for (;;) {
        bool enabled = atomic_load(&wake_enabled) && net_client_connected();
        if (detecting && !enabled) {
            wake_word_stop();
            detecting = false;
        }
        job_t job;
        if (xQueueReceive(jobs, &job, enabled ? 0 : portMAX_DELAY) != pdPASS) {
            if (!enabled) continue;
            if (!detecting) {
                detecting = wake_word_start();
                drain_before_wake = true;
                if (!detecting) {
                    ESP_LOGW("media", "WakeNet model unavailable");
                    atomic_store(&wake_enabled, false);
                    continue;
                }
            }
            bool detected = false;
            esp_err_t err = drain_before_wake ? audio_drain_input() : wake_word_poll(&detected);
            drain_before_wake = false;
            if (err != ESP_OK) {
                ESP_LOGW("media", "Disabling wake word after microphone error");
                atomic_store(&wake_enabled, false);
            }
            int64_t now = esp_timer_get_time();
            if (detected && now >= wake_after_us && atomic_load(&wake_enabled) &&
                !media_is_recording() && net_client_wake_word()) {
                display_wake();
                wake_after_us = now + 2000000;
            }
            continue;
        }
        if (job.kind == WAKE_CONTROL) continue;
        if (detecting) wake_word_reset();
        drain_before_wake = true;
        // The producer publishes cancellation and ownership before the worker uses the job.
        xSemaphoreTake(lock, portMAX_DELAY);
        xSemaphoreGive(lock);
        if (job.kind == RECORD) {
            record(&job);
        } else if (job.kind == CHIME) {
            audio_tone(job.tone, chime_interrupted, &job);
        } else {
            play();
        }
    }
}

esp_err_t media_init(bool audio_ready)
{
    if (!audio_ready) return ESP_OK;
    uint8_t *storage = heap_caps_malloc(TTS_BYTES + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!storage) return ESP_ERR_NO_MEM;
    tts = xStreamBufferCreateStatic(TTS_BYTES + 1, sizeof(int16_t), storage, &tts_control);
    lock = xSemaphoreCreateMutex();
    jobs = xQueueCreate(6, sizeof(job_t));
    return tts && lock && jobs && xTaskCreatePinnedToCore(worker, "media", 7168, NULL, 6, &media_task, 1) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}
