#include "media.h"

#include <math.h>
#include <stdatomic.h>
#include <stdio.h>
#include <string.h>

#include "display.h"
#include "debug_parse.h"
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
#include "net_logic.h"

#include "audio_logic.h"
#include "telemetry_logic.h"

enum {
    // The server keeps terminal_turn.maxAudioLead at 8 s and may prefill up to
    // it; the ring must absorb that plus scheduling jitter without aborting.
    TTS_LEAD_MS = 8000,
    TTS_BUFFER_MS = 12000,
    TTS_BYTES = AUDIO_RATE * (TTS_BUFFER_MS / 1000) * (int)sizeof(int16_t),
    TTS_BLOCK = 1280,
    TTS_SEND_WAIT_MS = 250,
};
_Static_assert(TTS_BUFFER_MS * 2 >= TTS_LEAD_MS * 3, "TTS ring must hold at least 1.5x the server audio lead");

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
    net_upload_t upload;
} job_t;

static QueueHandle_t jobs;
static SemaphoreHandle_t lock, player_done;
static TaskHandle_t media_task;
static StreamBufferHandle_t tts;
static StaticStreamBuffer_t tts_control;
// A queued STREAM job is a wakeup for the latest generation, not a second buffer owner.
static struct {
    atomic_uint serial;
    atomic_bool open, ended, playing;
    uint32_t session, audio_id;
    bool queued;
} stream;
static atomic_bool recording, stop_recording, cancel_recording;
static atomic_uint chime_epoch;
static atomic_bool tts_await_first;
static atomic_uint_fast64_t tts_first_byte_us;
static SemaphoreHandle_t debug_mic_mu;
static uint8_t *debug_mic_buf;
static debug_mic_state_t debug_mic;

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
    bool busy = recording || atomic_load_explicit(&stream.open, memory_order_acquire);
    xSemaphoreGive(lock);
    if (busy) return false;

    // A player that was interrupted may still be unwinding; resetting the ring
    // underneath it would race the receive it is about to leave.
    while (atomic_load_explicit(&stream.playing, memory_order_acquire)) {
        if (xSemaphoreTake(player_done, pdMS_TO_TICKS(500)) != pdPASS) return false;
    }

    xSemaphoreTake(lock, portMAX_DELAY);
    uint32_t session = net_client_session();
    bool accepted = session && !recording && !atomic_load_explicit(&stream.open, memory_order_acquire) &&
                    xStreamBufferReset(tts) == pdPASS &&
                    (stream.queued || submit(&(job_t){ .kind = STREAM }));
    if (accepted) {
        atomic_fetch_add_explicit(&stream.serial, 1, memory_order_release);
        stream.session = session;
        stream.audio_id = audio_id;
        stream.queued = true;
        atomic_store_explicit(&stream.ended, false, memory_order_release);
        atomic_store_explicit(&stream.open, true, memory_order_release);
        atomic_store_explicit(&tts_await_first, true, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.tts_samples, 0, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.tts_gap_ms, 0, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.tts_rms, 0, memory_order_relaxed);
        atomic_store_explicit(&telemetry_io.tts_first_us, 0, memory_order_relaxed);
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
    // Backpressure: hold the TCP read until the player drains room, but keep
    // reading control messages when a stuck player outlives the wait.
    for (unsigned waited = 0; xStreamBufferSpacesAvailable(tts) < bytes; waited += 5) {
        if (!atomic_load_explicit(&stream.open, memory_order_acquire) ||
            atomic_load_explicit(&stream.ended, memory_order_acquire)) {
            return false;
        }
        if (waited >= TTS_SEND_WAIT_MS) {
            // Drop this whole packet rather than aborting an audible reply.
            atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
            return true;
        }
        vTaskDelay(pdMS_TO_TICKS(5));
    }
    if (xStreamBufferSend(tts, data, bytes, 0) != bytes) {
        atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
        return true;
    }
    if (atomic_exchange_explicit(&tts_await_first, false, memory_order_relaxed)) {
        atomic_store_explicit(&tts_first_byte_us, (uint64_t)esp_timer_get_time(), memory_order_relaxed);
    }
    xTaskNotifyGive(media_task);
    return true;
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
    job_t *job = ctx;
    if (atomic_load(&cancel_recording)) return ESP_FAIL;
    bool sent = net_client_rec_pcm(job->session, pcm, samples);
    switch (net_upload_result(&job->upload, sent, NET_UPLOAD_FAIL_LIMIT)) {
    case NET_UPLOAD_SENT:
        return ESP_OK;
    case NET_UPLOAD_DROPPED:
        atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
        return ESP_OK;
    case NET_UPLOAD_CANCELLED:
        return ESP_FAIL;
    }
    return ESP_FAIL;
}

static void record(job_t *job)
{
    net_upload_init(&job->upload);
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
    return atomic_load_explicit(&stream.open, memory_order_acquire) &&
           atomic_load_explicit(&stream.serial, memory_order_relaxed) == serial;
}

static void play(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    stream.queued = false;
    bool open = atomic_load_explicit(&stream.open, memory_order_acquire);
    uint32_t serial = atomic_load_explicit(&stream.serial, memory_order_relaxed);
    if (open) {
        atomic_store_explicit(&stream.playing, true, memory_order_release);
        audio_stream_begin();
    }
    xSemaphoreGive(lock);
    if (!open) return;

    display_wake();
    int16_t chunk[TTS_BLOCK / sizeof(int16_t)];
    esp_err_t err = ESP_OK;
    bool played = false, starved = false;
    uint64_t sum_sq = 0;
    uint32_t total_samples = 0;
    telemetry_gap_t gap = { 0 };
    while (err == ESP_OK) {
        bool owns = owns_stream(serial);
        size_t bytes = owns ? xStreamBufferReceive(tts, chunk, sizeof(chunk), 0) : 0;
        bool done = !owns || (atomic_load_explicit(&stream.ended, memory_order_acquire) && !bytes);
        if (done) break;
        int64_t now = esp_timer_get_time();
        if (bytes % sizeof(*chunk)) {
            err = ESP_ERR_INVALID_SIZE;
        } else if (bytes) {
            size_t n = bytes / sizeof(*chunk);
            err = audio_stream_write(chunk, n);
            if (err == ESP_OK) {
                if (!played) {
                    uint64_t dt = (uint64_t)now - atomic_load_explicit(&tts_first_byte_us, memory_order_relaxed);
                    atomic_store_explicit(&telemetry_io.tts_first_us, (unsigned)dt, memory_order_relaxed);
                }
                for (size_t i = 0; i < n; ++i) {
                    int64_t s = chunk[i];
                    sum_sq += (uint64_t)(s * s);
                }
                total_samples += (uint32_t)n;
                // Publish as the stream plays so a server fallback can tell a
                // completed ack from a stalled one without waiting for the end.
                atomic_store_explicit(&telemetry_io.tts_samples, total_samples, memory_order_relaxed);
                telemetry_gap_update(&gap, now, false);
                atomic_store_explicit(&telemetry_io.tts_gap_ms, gap.total_ms, memory_order_relaxed);
                played = true;
                starved = false;
            }
        } else {
            // Count starvation once per gap after playback has started, not
            // the initial prefill or the normal drain after an end frame.
            if (played && !starved) {
                atomic_fetch_add_explicit(&telemetry_io.audio_underruns, 1, memory_order_relaxed);
                starved = true;
            }
            if (played) {
                telemetry_gap_update(&gap, now, true);
                atomic_store_explicit(&telemetry_io.tts_gap_ms, gap.total_ms, memory_order_relaxed);
            }
            ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
        }
    }
    if (played) {
        telemetry_gap_close(&gap, esp_timer_get_time());
        atomic_store_explicit(&telemetry_io.tts_gap_ms, gap.total_ms, memory_order_relaxed);
    }
    if (total_samples > 0) {
        uint32_t rms = (uint32_t)sqrt((double)sum_sq / (double)total_samples);
        atomic_store_explicit(&telemetry_io.tts_rms, rms, memory_order_relaxed);
    }
    esp_err_t end = audio_stream_end();
    xSemaphoreTake(lock, portMAX_DELAY);
    if (owns_stream(serial)) {
        atomic_store_explicit(&stream.open, false, memory_order_release);
        net_client_tts_ack(stream.session, stream.audio_id, err != ESP_OK || end != ESP_OK);
    }
    atomic_store_explicit(&stream.playing, false, memory_order_release);
    xSemaphoreGive(player_done);
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

uint32_t media_debug_mic_begin(void)
{
    if (!debug_mic_mu) return 0;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    uint32_t gen = debug_mic_invalidate(&debug_mic);
    xSemaphoreGive(debug_mic_mu);
    return gen;
}

bool media_debug_mic_load(uint32_t gen, size_t offset, const uint8_t *data, size_t len, size_t total_len, bool commit)
{
    if (!debug_mic_mu || (len && !data)) return false;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    bool ok = false;
    if (!debug_mic_buf) {
        debug_mic_buf = heap_caps_malloc(DEBUG_MIC_MAX_BYTES, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    }
    // A superseded upload (gen mismatch), a truncated body or an odd byte count
    // never becomes readable as committed PCM.
    if (debug_mic_buf && debug_mic_load_ok(&debug_mic, gen, offset, len, total_len, DEBUG_MIC_MAX_BYTES, commit)) {
        if (len) memcpy(debug_mic_buf + offset, data, len);
        ok = true;
    }
    xSemaphoreGive(debug_mic_mu);
    return ok;
}

void media_debug_mic_abort(uint32_t gen)
{
    if (!debug_mic_mu) return;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    debug_mic_abort(&debug_mic, gen);
    xSemaphoreGive(debug_mic_mu);
}

bool media_debug_mic_active(void)
{
    if (!debug_mic_mu) return false;
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    bool active = debug_mic.pos < debug_mic.len;
    xSemaphoreGive(debug_mic_mu);
    return active;
}

bool media_debug_mic_read_10ms(int16_t *dst)
{
    if (!debug_mic_mu || !debug_mic_buf || !dst) return false;
    const size_t frame_bytes = AUDIO_10MS * sizeof(int16_t);
    xSemaphoreTake(debug_mic_mu, portMAX_DELAY);
    if (debug_mic.pos >= debug_mic.len) {
        xSemaphoreGive(debug_mic_mu);
        return false;
    }
    size_t avail = debug_mic.len - debug_mic.pos;
    size_t copy = avail < frame_bytes ? avail : frame_bytes;
    memcpy(dst, debug_mic_buf + debug_mic.pos, copy);
    if (copy < frame_bytes) {
        memset((uint8_t *)dst + copy, 0, frame_bytes - copy);
    }
    debug_mic.pos += copy;
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
    player_done = xSemaphoreCreateBinary();
    jobs = xQueueCreate(6, sizeof(job_t));
    return tts && lock && jobs && player_done &&
                   xTaskCreatePinnedToCore(worker, "media", 7168, NULL, 6, &media_task, 1) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}
