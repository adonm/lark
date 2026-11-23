#include "telemetry_logic.h"

#include <assert.h>
#include <stdio.h>

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
    test_gap_duration();
    puts("PASS CPU sampling and TTS gap-duration accounting");
    return 0;
}
