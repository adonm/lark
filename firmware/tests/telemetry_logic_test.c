#include "telemetry_logic.h"

#include <assert.h>
#include <stdio.h>
#include <string.h>

static void test_cpu_pct(void)
{
    // No window yet and fully idle both report zero.
    assert(telemetry_cpu_pct(0, 0) == 0);
    assert(telemetry_cpu_pct(100000, 100000) == 0);
    // Two cores busy for half the window are 50%.
    assert(telemetry_cpu_pct(2000000, 1000000) == 50);
    assert(telemetry_cpu_pct(2000000, 0) == 100);
    // Rounding is to the nearest percent, and counter underflow cannot exceed 100.
    assert(telemetry_cpu_pct(1000, 995) == 1);
    assert(telemetry_cpu_pct(1000, 990) == 1);
    assert(telemetry_cpu_pct(1000, 1) == 100);
    assert(telemetry_cpu_pct(1000, 2000) == 0);
}

static void test_reset_reason_and_cadence(void)
{
    assert(strcmp(telemetry_reset_reason(1), "poweron") == 0);
    assert(strcmp(telemetry_reset_reason(3), "sw") == 0);
    assert(strcmp(telemetry_reset_reason(4), "panic") == 0);
    assert(strcmp(telemetry_reset_reason(6), "task_wdt") == 0);
    assert(strcmp(telemetry_reset_reason(9), "brownout") == 0);
    assert(strcmp(telemetry_reset_reason(11), "usb") == 0);
    assert(strcmp(telemetry_reset_reason(12), "jtag") == 0);
    assert(strcmp(telemetry_reset_reason(0), "other") == 0);
    assert(strcmp(telemetry_reset_reason(2), "other") == 0);

    assert(telemetry_status_interval_ms(false) == 5000);
    assert(telemetry_status_interval_ms(true) == 15000);
    // The sleeping cadence must stay inside the edge's 75 s read deadline.
    assert(telemetry_status_interval_ms(true) < 75000);
}

static void test_gap_duration(void)
{
    telemetry_gap_t gap = { 0 };
    telemetry_gap_update(&gap, 1000000, false);
    assert(gap.total_ms == 0 && gap.gap_start_us == 0);

    // One 250 ms underrun.
    telemetry_gap_update(&gap, 2000000, true);
    telemetry_gap_update(&gap, 2250000, false);
    assert(gap.total_ms == 250);
    assert(gap.gap_start_us == 0);

    // Repeated starving samples do not restart or double-count the gap.
    telemetry_gap_update(&gap, 3000000, true);
    telemetry_gap_update(&gap, 3050000, true);
    telemetry_gap_update(&gap, 3100000, true);
    telemetry_gap_update(&gap, 3175000, false);
    assert(gap.total_ms == 250 + 175);

    // An open gap is closed when the stream ends.
    telemetry_gap_update(&gap, 4000000, true);
    telemetry_gap_close(&gap, 4040000);
    assert(gap.total_ms == 250 + 175 + 40);
    telemetry_gap_close(&gap, 5000000);
    assert(gap.total_ms == 250 + 175 + 40);
}

int main(void)
{
    test_cpu_pct();
    test_reset_reason_and_cadence();
    test_gap_duration();
    puts("PASS CPU sampling, reset reasons, cadence and TTS gap accounting");
    return 0;
}
