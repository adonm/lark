#include "display_logic.h"

#include <assert.h>
#include <stdio.h>

static void test_full_height_strips(void)
{
    frame_bounds_t area = { 0, 0, FRAME_WIDTH, FRAME_HEIGHT };
    int count = 0, covered = 0, last_rows = 0;
    for (int y = area.y0; y < area.y1; y += last_rows) {
        last_rows = display_strip_rows(area, y, DISPLAY_STRIP_ROWS);
        assert(last_rows > 0 && last_rows <= DISPLAY_STRIP_ROWS);
        covered += last_rows;
        ++count;
    }
    assert(covered == FRAME_HEIGHT);
    assert(count == (FRAME_HEIGHT + DISPLAY_STRIP_ROWS - 1) / DISPLAY_STRIP_ROWS);
}

static void test_partial_area(void)
{
    frame_bounds_t area = { 0, 11, FRAME_WIDTH, FRAME_HEIGHT - 10 };
    int y = area.y0, strips = 0, covered = 0, last = 0;
    while (y < area.y1) {
        int rows = display_strip_rows(area, y, DISPLAY_STRIP_ROWS);
        assert(rows > 0 && rows <= DISPLAY_STRIP_ROWS);
        y += rows;
        covered += rows;
        ++strips;
        last = rows;
    }
    assert(y == area.y1);
    assert(covered == area.y1 - area.y0);
    assert(last == (area.y1 - area.y0) % DISPLAY_STRIP_ROWS);
    assert(strips == (area.y1 - area.y0 + DISPLAY_STRIP_ROWS - 1) / DISPLAY_STRIP_ROWS);

    // The iterator reports the same first strip and refuses an empty range.
    int it_y = area.y0, it_rows = 0;
    assert(display_strip_next(area, &it_y, &it_rows, DISPLAY_STRIP_ROWS));
    assert(it_y == area.y0 && it_rows == DISPLAY_STRIP_ROWS);
    frame_bounds_t empty = { 0, 20, FRAME_WIDTH, 20 };
    it_y = 20;
    assert(!display_strip_next(empty, &it_y, &it_rows, DISPLAY_STRIP_ROWS));
}

static void test_wider_strips(void)
{
    frame_bounds_t area = { 0, 0, FRAME_WIDTH, 240 };
    for (int rows_per_strip = 120; rows_per_strip <= 160; rows_per_strip += 40) {
        int y = area.y0, covered = 0, strips = 0;
        while (y < area.y1) {
            int rows = display_strip_rows(area, y, rows_per_strip);
            assert(rows > 0 && rows <= rows_per_strip);
            y += rows;
            covered += rows;
            ++strips;
        }
        assert(covered == 240);
        assert(strips == (240 + rows_per_strip - 1) / rows_per_strip);
    }
    // Out-of-range starts produce no work.
    assert(display_strip_rows(area, -1, DISPLAY_STRIP_ROWS) == 0);
    assert(display_strip_rows(area, area.y1, DISPLAY_STRIP_ROWS) == 0);
    assert(display_strip_rows(area, 0, 0) == 0);
}

int main(void)
{
    test_full_height_strips();
    test_partial_area();
    test_wider_strips();
    // Dimming is an offline fallback only; a linked host owns panel power.
    assert(display_idle_dimmed(false, 300001, 180001, DISPLAY_IDLE_DIM_MS));
    assert(!display_idle_dimmed(false, 300000, 180001, DISPLAY_IDLE_DIM_MS));
    assert(!display_idle_dimmed(true, 300001, 180001, DISPLAY_IDLE_DIM_MS));
    puts("PASS display strip scheduling covers both geometries");
    return 0;
}
