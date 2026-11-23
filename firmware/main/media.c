#include "media.h"

#include <math.h>
#include <stdatomic.h>
#include <stdio.h>
#include <string.h>

#include "display.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"
#include "telemetry.h"
#include "net_client.h"

enum { TTS_BYTES = 256 * 1024, TTS_BLOCK = 1280 };
typedef enum { RECORD, STREAM, CHIME } job_kind_t;
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
    atomic_bool open, ended;
    bool queued;
} stream;
static atomic_bool recording, stop_recording, cancel_recording;
static atomic_uint chime_epoch;
static atomic_bool tts_await_first;
static atomic_uint_fast64_t tts_first_byte_us;
static SemaphoreHandle_t debug_mic_mu;
static uint8_t *debug_mic_buf;
static size_t debug_mic_len, debug_mic_pos;

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

static void interrupt_locked(void)
{
    if (atomic_load_explicit(&stream.open, memory_order_acquire)) {
        audio_stop_playback();
        atomic_store_explicit(&stream.open, false, memory_order_release);
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
    bool open = atomic_load_explicit(&stream.open, memory_order_acquire);
    bool accepted = session && !recording && !open && xStreamBufferReset(tts) == pdPASS &&
                    (stream.queued || submit(&(job_t){ .kind = STREAM }));
    if (accepted) {
        ++stream.serial;
        stream.session = session;
        stream.audio_id = audio_id;
        stream.queued = true;
        atomic_store_explicit(&stream.ended, false, memory_order_release);
        atomic_store_explicit(&stream.open, true, memory_order_release);
        atomic_store_explicit(&tts_await_first, true, memory_order_relaxed);
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_tts_end(void)
{
    if (atomic_load_explicit(&stream.open, memory_order_acquire)) {
        atomic_store_explicit(&stream.ended, true, memory_order_release);
        xTaskNotifyGive(media_task);
    }
}

bool media_tts_pcm(const void *data, size_t bytes)
{
    if (!atomic_load_explicit(&stream.open, memory_order_acquire) ||
        atomic_load_explicit(&stream.ended, memory_order_acquire)) {
        return false;
    }
    // Single-producer/single-consumer StreamBuffer: lock-free between net_service and media.
    if (xStreamBufferSpacesAvailable(tts) >= bytes && xStreamBufferSend(tts, data, bytes, 0) == bytes) {
        if (atomic_exchange_explicit(&tts_await_first, false, memory_order_relaxed)) {
            atomic_store_explicit(&tts_first_byte_us, (uint64_t)esp_timer_get_time(), memory_order_relaxed);
        }
        xTaskNotifyGive(media_task);
        return true;
    }
    atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
    media_interrupt();
    return false;
}

void media_chime(audio_tone_t tone)
{
    if (!jobs) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (!recording) {
        if (atomic_load_explicit(&stream.open, memory_order_acquire) && tone == AUDIO_HAPPY_TONE) {
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
    if (started && !capture_cancelled(NULL)) {
        esp_err_t err = audio_record_stream(job->bridge_endpoint, job->listen, net_client_rec_buf(),
                                            stop_capture, upload_pcm, job);
        cancelled = err != ESP_OK || capture_cancelled(NULL);
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    recording = false;
    xSemaphoreGive(lock);
    if (started) net_client_rec_end(job->session, job->rec_session, job->target, cancelled);
}

static bool owns_stream(uint32_t serial)
{
    return atomic_load_explicit(&stream.open, memory_order_acquire) && stream.serial == serial;
}

static void play(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    stream.queued = false;
    bool open = atomic_load_explicit(&stream.open, memory_order_acquire);
    uint32_t serial = stream.serial;
    if (open) audio_stream_begin();
    xSemaphoreGive(lock);
    if (!open) return;

    display_wake();
    int16_t chunk[TTS_BLOCK / sizeof(int16_t)];
    esp_err_t err = ESP_OK;
    bool played = false, starved = false;
    uint64_t sum_sq = 0;
    uint32_t total_samples = 0;
    while (err == ESP_OK) {
        bool owns = owns_stream(serial);
        size_t bytes = owns ? xStreamBufferReceive(tts, chunk, sizeof(chunk), 0) : 0;
        bool done = !owns || (atomic_load_explicit(&stream.ended, memory_order_acquire) && !bytes);
        if (done) break;
        if (bytes % sizeof(*chunk)) {
            err = ESP_ERR_INVALID_SIZE;
        } else if (bytes) {
            if (!played) {
                uint64_t dt = (uint64_t)esp_timer_get_time() -
                              atomic_load_explicit(&tts_first_byte_us, memory_order_relaxed);
                atomic_store_explicit(&telemetry_io.tts_first_us, (unsigned)dt, memory_order_relaxed);
            }
            size_t n = bytes / sizeof(*chunk);
            for (size_t i = 0; i < n; ++i) {
                int64_t s = chunk[i];
                sum_sq += (uint64_t)(s * s);
            }
            total_samples += (uint32_t)n;
            err = audio_stream_write(chunk, n);
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
    if (total_samples > 0) {
        uint32_t rms = (uint32_t)sqrt((double)sum_sq / (double)total_samples);
        atomic_store_explicit(&telemetry_io.tts_rms, rms, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.tts_samples, total_samples, memory_order_relaxed);
    }
    esp_err_t end = audio_stream_end();
    xSemaphoreTake(lock, portMAX_DELAY);
    if (owns_stream(serial)) {
        atomic_store_explicit(&stream.open, false, memory_order_release);
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
    for (;;) {
        job_t job;
        if (xQueueReceive(jobs, &job, portMAX_DELAY) != pdPASS) continue;
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

bool media_debug_mic_load(size_t offset, const uint8_t *data, size_t len, size_t total_len, bool commit)
{
    if (!debug_mic_mu || total_len > DEBUG_MIC_MAX_BYTES || offset + len > total_len) return false;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    if (!debug_mic_buf) {
        debug_mic_buf = heap_caps_malloc(DEBUG_MIC_MAX_BYTES, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
        if (!debug_mic_buf) {
            xSemaphoreGive(debug_mic_mu);
            return false;
        }
    }
    if (offset == 0) {
        debug_mic_len = 0;
        debug_mic_pos = 0;
    }
    if (len > 0 && data) {
        memcpy(debug_mic_buf + offset, data, len);
    }
    if (commit) {
        debug_mic_len = total_len;
        debug_mic_pos = 0;
    }
    xSemaphoreGive(debug_mic_mu);
    return true;
}

bool media_debug_mic_active(void)
{
    if (!debug_mic_mu) return false;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    bool active = debug_mic_pos < debug_mic_len;
    xSemaphoreGive(debug_mic_mu);
    return active;
}

bool media_debug_mic_read_10ms(int16_t *dst)
{
    if (!debug_mic_mu || !debug_mic_buf || !dst) return false;
    const size_t frame_bytes = AUDIO_10MS * sizeof(int16_t);
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    if (debug_mic_pos >= debug_mic_len) {
        xSemaphoreGive(debug_mic_mu);
        return false;
    }
    size_t avail = debug_mic_len - debug_mic_pos;
    size_t copy = avail < frame_bytes ? avail : frame_bytes;
    memcpy(dst, debug_mic_buf + debug_mic_pos, copy);
    if (copy < frame_bytes) {
        memset((uint8_t *)dst + copy, 0, frame_bytes - copy);
    }
    debug_mic_pos += copy;
    xSemaphoreGive(debug_mic_mu);
    return true;
}

esp_err_t media_init(bool audio_ready)
{
    if (!audio_ready) return ESP_OK;
    uint8_t *storage = heap_caps_malloc(TTS_BYTES + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    debug_mic_mu = xSemaphoreCreateMutex();
    if (!storage || !debug_mic_mu) return ESP_ERR_NO_MEM;
    tts = xStreamBufferCreateStatic(TTS_BYTES + 1, sizeof(int16_t), storage, &tts_control);
    lock = xSemaphoreCreateMutex();
    jobs = xQueueCreate(6, sizeof(job_t));
    return tts && lock && jobs && xTaskCreatePinnedToCore(worker, "media", 7168, NULL, 6, &media_task, 1) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}
