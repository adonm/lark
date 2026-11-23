#include "telemetry_logic.h"

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
