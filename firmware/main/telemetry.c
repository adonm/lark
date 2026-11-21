#include "telemetry.h"

#include "esp_heap_caps.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static portMUX_TYPE cpu_lock = portMUX_INITIALIZER_UNLOCKED;
static uint64_t sampled_us, idle_us;
static unsigned cpu_pct;
struct telemetry_io telemetry_io;

static unsigned sample_cpu(uint64_t *uptime_us)
{
    portENTER_CRITICAL(&cpu_lock);
    uint64_t now = esp_timer_get_time();
    uint64_t elapsed = now - sampled_us;
    // Share a five-second minimum sample window between HTTP and bridge readers.
    if (elapsed && (!sampled_us || elapsed >= 5000000)) {
        uint64_t idle = 0;
        for (int core = 0; core < configNUMBER_OF_CORES; ++core) {
            idle += ulTaskGetIdleRunTimeCounterForCore(core);
        }
        uint64_t capacity = elapsed * configNUMBER_OF_CORES;
        uint64_t idle_delta = idle - idle_us;
        cpu_pct = idle_delta >= capacity ? 0 : ((capacity - idle_delta) * 100 + capacity / 2) / capacity;
        sampled_us = now;
        idle_us = idle;
    }
    unsigned result = cpu_pct;
    portEXIT_CRITICAL(&cpu_lock);
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
        .call_tx_drops = atomic_load_explicit(&telemetry_io.call_tx_drops, memory_order_relaxed),
    };
}
