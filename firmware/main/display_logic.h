#pragma once

#include "frame.h"

#include <stdbool.h>

// Strip planning for the LCD blitter. STRIP_ROWS rows are copied into a DMA
// buffer and sent, so the last strip of a dirty area may be shorter.
enum { DISPLAY_STRIP_ROWS = 24, DISPLAY_IDLE_DIM_MS = 120000 };

// Rows for the strip starting at y, clamped to the half-open dirty y range.
int display_strip_rows(frame_bounds_t area, int y, int strip_rows);

// Iterates the strips of a dirty area. Returns false when y reaches area.y1.
bool display_strip_next(frame_bounds_t area, int *y, int *rows, int strip_rows);

// Local dimming is an offline fallback; a linked host owns panel power.
bool display_idle_dimmed(bool linked, uint32_t now_ms, uint32_t last_touch_ms, uint32_t idle_ms);
