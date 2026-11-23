#include "net_logic.h"

#include <assert.h>
#include <stdio.h>

static void test_backoff_needs_welcome_to_reset(void)
{
    net_backoff_t b;
    net_backoff_init(&b);
    assert(net_backoff_delay_ms(&b) == NET_BACKOFF_MIN_MS);
    const int want[] = { 2000, 4000, 8000, 16000, 30000, 30000 };
    for (size_t i = 0; i < sizeof(want) / sizeof(want[0]); ++i) {
        net_backoff_failed(&b);
        assert(net_backoff_delay_ms(&b) == want[i]);
    }
    assert(!net_backoff_stopped(&b));

    // A drop before WELCOME keeps the grown delay; only WELCOME restarts it.
    net_backoff_failed(&b);
    assert(net_backoff_delay_ms(&b) == NET_BACKOFF_MAX_MS);
    net_backoff_connected(&b);
    assert(net_backoff_delay_ms(&b) == NET_BACKOFF_MIN_MS);
    assert(!net_backoff_stopped(&b));
}

static void test_terminal_reject_stops_until_reinit(void)
{
    net_backoff_t b;
    net_backoff_init(&b);
    net_backoff_rejected(&b, true);
    assert(net_backoff_stopped(&b));
    net_backoff_connected(&b);
    net_backoff_failed(&b);
    assert(net_backoff_stopped(&b));

    net_backoff_init(&b);
    net_backoff_rejected(&b, false);
    assert(!net_backoff_stopped(&b));
    assert(net_backoff_delay_ms(&b) == 2000);
}

static void test_upload_drops_until_limit(void)
{
    net_upload_t u;
    net_upload_init(&u);
    for (unsigned i = 1; i < NET_UPLOAD_FAIL_LIMIT; ++i) {
        assert(net_upload_result(&u, false, NET_UPLOAD_FAIL_LIMIT) == NET_UPLOAD_DROPPED);
    }
    assert(net_upload_result(&u, true, NET_UPLOAD_FAIL_LIMIT) == NET_UPLOAD_SENT);
    // One good datagram resets the consecutive-failure budget.
    for (unsigned i = 1; i < NET_UPLOAD_FAIL_LIMIT; ++i) {
        assert(net_upload_result(&u, false, NET_UPLOAD_FAIL_LIMIT) == NET_UPLOAD_DROPPED);
    }
    assert(net_upload_result(&u, false, NET_UPLOAD_FAIL_LIMIT) == NET_UPLOAD_CANCELLED);
    assert(net_upload_result(&u, true, NET_UPLOAD_FAIL_LIMIT) == NET_UPLOAD_SENT);
}

static void test_ack_newest_stream_wins(void)
{
    net_ack_t ack = { 0 }, out;
    assert(!net_ack_take(&ack, &out));
    net_ack_submit(&ack, 4, 10, false);
    assert(net_ack_take(&ack, &out) && out.session == 4 && out.audio_id == 10 && !out.aborted);
    // Still pending until the frame is sent.
    assert(net_ack_take(&ack, &out));
    net_ack_sent(&ack);
    assert(!net_ack_take(&ack, &out));

    // A late abort cannot retract a queued completion.
    net_ack_submit(&ack, 4, 10, false);
    net_ack_submit(&ack, 4, 10, true);
    assert(net_ack_take(&ack, &out) && !out.aborted);

    // A newer stream replaces an unsent ack, including an aborted one.
    net_ack_submit(&ack, 4, 10, true);
    net_ack_submit(&ack, 4, 11, false);
    assert(net_ack_take(&ack, &out) && out.audio_id == 11 && !out.aborted);

    // A new socket session invalidates the old stream's ack.
    net_ack_submit(&ack, 5, 12, true);
    assert(net_ack_take(&ack, &out) && out.session == 5 && out.aborted);

    // Invalid ids never enter the slot.
    net_ack_sent(&ack);
    net_ack_submit(&ack, 0, 12, false);
    net_ack_submit(&ack, 5, 0, false);
    assert(!net_ack_take(&ack, &out));
}

int main(void)
{
    test_backoff_needs_welcome_to_reset();
    test_terminal_reject_stops_until_reinit();
    test_upload_drops_until_limit();
    test_ack_newest_stream_wins();
    puts("PASS reconnect backoff, upload drops and TTS ack policy");
    return 0;
}
