#include "telemetry_logic.h"

const char *telemetry_reset_reason(int reason)
{
    switch (reason) {
    case 1: return "poweron";    // ESP_RST_POWERON
    case 3: return "sw";         // ESP_RST_SW
    case 4: return "panic";      // ESP_RST_PANIC
    case 5: return "int_wdt";    // ESP_RST_INT_WDT
    case 6: return "task_wdt";   // ESP_RST_TASK_WDT
    case 7: return "wdt";        // ESP_RST_WDT
    case 8: return "deepsleep";  // ESP_RST_DEEPSLEEP
    case 9: return "brownout";   // ESP_RST_BROWNOUT
    case 10: return "sdio";      // ESP_RST_SDIO
    case 11: return "usb";       // ESP_RST_USB
    case 12: return "jtag";      // ESP_RST_JTAG
    default: return "other";
    }
}

int telemetry_status_interval_ms(bool sleeping)
{
    return sleeping ? TELEMETRY_STATUS_SLEEPING_MS : TELEMETRY_STATUS_AWAKE_MS;
}

unsigned telemetry_cpu_pct(uint64_t capacity_us, uint64_t idle_us)
{
    if (!capacity_us || idle_us >= capacity_us) return 0;
    uint64_t busy = (capacity_us - idle_us) * 100 + capacity_us / 2;
    uint64_t pct = busy / capacity_us;
    return pct > 100 ? 100 : (unsigned)pct;
}

void telemetry_gap_update(telemetry_gap_t *gap, int64_t now_us, bool starving)
{
    if (!gap) return;
    if (!starving) {
        telemetry_gap_close(gap, now_us);
        return;
    }
    if (!gap->gap_start_us) gap->gap_start_us = now_us;
}

void telemetry_gap_close(telemetry_gap_t *gap, int64_t now_us)
{
    if (!gap || !gap->gap_start_us) return;
    if (now_us > gap->gap_start_us) {
        gap->total_ms += (uint32_t)((now_us - gap->gap_start_us) / 1000);
    }
    gap->gap_start_us = 0;
}
