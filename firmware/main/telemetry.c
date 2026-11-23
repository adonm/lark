#include "telemetry.h"

#include "telemetry_logic.h"

#include "esp_heap_caps.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static portMUX_TYPE cpu_lock = portMUX_INITIALIZER_UNLOCKED;
static uint64_t sampled_us, idle_us;
static unsigned cpu_pct;
static bool sampling;
struct telemetry_io telemetry_io;

static unsigned sample_cpu(uint64_t *uptime_us)
{
    uint64_t now = esp_timer_get_time();
    uint64_t previous_sampled = 0, previous_idle = 0;
    bool due = false;
    unsigned result = 0;
    portENTER_CRITICAL(&cpu_lock);
    result = cpu_pct;
    // Share a five-second minimum sample window between HTTP and bridge readers;
    // one reader computes while the other returns the last value.
    if (!sampling && (!sampled_us || now - sampled_us >= 5000000)) {
        due = true;
        sampling = true;
        previous_sampled = sampled_us;
        previous_idle = idle_us;
        sampled_us = now;
    }
    portEXIT_CRITICAL(&cpu_lock);

    if (due) {
        // Idle-run-time counters are sampled outside the lock, where they
        // cannot block an ISR or another status reader.
        uint64_t idle = 0;
        for (int core = 0; core < configNUMBER_OF_CORES; ++core) {
            idle += ulTaskGetIdleRunTimeCounterForCore(core);
        }
        uint64_t idle_delta = idle >= previous_idle ? idle - previous_idle : 0;
        result = telemetry_cpu_pct((now - previous_sampled) * configNUMBER_OF_CORES, idle_delta);
        portENTER_CRITICAL(&cpu_lock);
        idle_us = idle;
        cpu_pct = result;
        sampling = false;
        portEXIT_CRITICAL(&cpu_lock);
    }
    *uptime_us = now;
    return result;
}

telemetry_t telemetry_read(void)
{
    uint64_t uptime_us;
    unsigned cpu = sample_cpu(&uptime_us);
    wifi_ap_record_t ap;
    return (telemetry_t){
        .free_heap = heap_caps_get_free_size(MALLOC_CAP_INTERNAL),
        .min_free_heap = heap_caps_get_minimum_free_size(MALLOC_CAP_INTERNAL),
        .free_psram = heap_caps_get_free_size(MALLOC_CAP_SPIRAM),
        .uptime_s = uptime_us / 1000000,
        .wifi_rssi = esp_wifi_sta_get_ap_info(&ap) == ESP_OK ? ap.rssi : 0,
        .cpu_pct = cpu,
        .last_frame_rev = atomic_load_explicit(&telemetry_io.last_frame_rev, memory_order_relaxed),
        .display_flush_ms = atomic_load_explicit(&telemetry_io.display_flush_ms, memory_order_relaxed),
        .input_drops = atomic_load_explicit(&telemetry_io.input_drops, memory_order_relaxed),
        .audio_underruns = atomic_load_explicit(&telemetry_io.audio_underruns, memory_order_relaxed),
        .audio_drops = atomic_load_explicit(&telemetry_io.audio_drops, memory_order_relaxed),
        .rec_start_us = atomic_load_explicit(&telemetry_io.rec_start_us, memory_order_relaxed),
        .tts_first_us = atomic_load_explicit(&telemetry_io.tts_first_us, memory_order_relaxed),
        .tts_gap_ms = atomic_load_explicit(&telemetry_io.tts_gap_ms, memory_order_relaxed),
        .ptr_send_us = atomic_load_explicit(&telemetry_io.ptr_send_us, memory_order_relaxed),
        .frame_flush_us = atomic_load_explicit(&telemetry_io.frame_flush_us, memory_order_relaxed),
        .tts_rms = atomic_load_explicit(&telemetry_io.tts_rms, memory_order_relaxed),
        .tts_samples = atomic_load_explicit(&telemetry_io.tts_samples, memory_order_relaxed),
    };
}
