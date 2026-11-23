#include "net_logic.h"

#include <assert.h>
#include <stdio.h>

static void test_backoff_doubles_and_caps(void)
{
    int ms = net_backoff_reset();
    assert(ms == NET_BACKOFF_MIN_MS);
    const int want[] = { 2000, 4000, 8000, 16000, 30000, 30000 };
    for (size_t i = 0; i < sizeof(want) / sizeof(want[0]); ++i) {
        ms = net_backoff_next(ms);
        assert(ms == want[i]);
    }
    // A reset after any failure restarts at one second.
    assert(net_backoff_reset() == NET_BACKOFF_MIN_MS);
    // Defensive: an out-of-range current value never shrinks below the cap.
    assert(net_backoff_next(-5) == 2000);
    assert(net_backoff_next(99999) == NET_BACKOFF_MAX_MS);
}

int main(void)
{
    test_backoff_doubles_and_caps();
    puts("PASS backoff doubles, caps at 30 s and resets");
    return 0;
}
