#include "display_logic.h"

int display_strip_rows(frame_bounds_t area, int y, int strip_rows)
{
    if (strip_rows <= 0 || y < area.y0 || y >= area.y1) return 0;
    int left = area.y1 - y;
    return left < strip_rows ? left : strip_rows;
}

bool display_strip_next(frame_bounds_t area, int *y, int *rows, int strip_rows)
{
    if (!y || !rows) return false;
    int start = *y < area.y0 ? area.y0 : *y;
    int n = display_strip_rows(area, start, strip_rows);
    if (!n) return false;
    *y = start;
    *rows = n;
    return true;
}

bool display_idle_dimmed(bool linked, uint32_t now_ms, uint32_t last_touch_ms, uint32_t idle_ms)
{
    return !linked && (uint32_t)(now_ms - last_touch_ms) >= idle_ms;
}
