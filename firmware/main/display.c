#include "display.h"
#include "display_logic.h"

#include <stdatomic.h>
#include <string.h>

#include "bsp/display.h"
#include "bsp/touch.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "frame.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "gfx.h"
#include "telemetry.h"
#include "touch_gesture.h"
#include "wifi.h"
#include "net_client.h"
#include "proto.h"

enum { STRIP_ROWS = DISPLAY_STRIP_ROWS, STRIP_BYTES = STRIP_ROWS * FRAME_WIDTH * sizeof(uint16_t), BRIGHT = 85, DIM = 12 };
_Static_assert(BSP_LCD_H_RES == FRAME_WIDTH && BSP_LCD_V_RES == FRAME_HEIGHT && BSP_LCD_BIGENDIAN,
               "Framebuffer must match the LCD geometry and byte order");
static const char *TAG = "display";
static esp_lcd_panel_handle_t panel;
static esp_lcd_touch_handle_t touch;
static SemaphoreHandle_t drawn, fb_lock;
static TaskHandle_t painter, touch_handle;
static atomic_uint inject_tap;
static uint16_t *fb, *strip[2];
static uint8_t *pending_pkt, *work_pkt;
static size_t pending_len;
static int64_t pending_rx_us, work_rx_us;
static bool linked;
static bool first_frame;
static frame_bounds_t dirty_bounds;
static atomic_int brightness = BRIGHT;
static atomic_uint last_touch;
static atomic_uint shown_rev;
static atomic_bool ps_dirty = true;
static atomic_bool touch_irq_ok;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

static bool color_done(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *event, void *ctx)
{
    BaseType_t awaken = pdFALSE;
    xSemaphoreGiveFromISR(ctx, &awaken);
    return awaken == pdTRUE;
}

static esp_err_t copy_strip(uint16_t *dst, frame_bounds_t area, int y, int rows)
{
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    bool ok = frame_copy_strip(dst, fb, area, y, rows);
    xSemaphoreGive(fb_lock);
    return ok ? ESP_OK : ESP_ERR_INVALID_ARG;
}

static esp_err_t blit(frame_bounds_t area)
{
    if (area.x0 >= area.x1 || area.y0 >= area.y1) return ESP_OK;
    int64_t started = esp_timer_get_time();
    int cur = 0;
    bool in_flight = false;
    esp_err_t err = ESP_OK;
    for (int y = area.y0; y < area.y1; ) {
        int rows = display_strip_rows(area, y, STRIP_ROWS);
        if (!rows) break;
        // Copy the next strip into strip[cur] while strip[cur ^ 1] transfers over SPI DMA.
        if ((err = copy_strip(strip[cur], area, y, rows)) != ESP_OK) break;
        if (in_flight) {
            xSemaphoreTake(drawn, portMAX_DELAY);
            in_flight = false;
        }
        err = esp_lcd_panel_draw_bitmap(panel, area.x0, y, area.x1, y + rows, strip[cur]);
        if (err != ESP_OK) {
            ESP_LOGE(TAG, "Queue LCD strip: %s", esp_err_to_name(err));
            break;
        }
        in_flight = true;
        cur ^= 1;
        y += rows;
    }
    if (in_flight) {
        xSemaphoreTake(drawn, portMAX_DELAY);
    }
    atomic_store_explicit(&telemetry_io.display_flush_ms, (esp_timer_get_time() - started) / 1000, memory_order_relaxed);
    return err;
}

static void display_show_offline(void)
{
    gfx_offline(fb);
}

static void paint_task(void *arg)
{
    int applied = BRIGHT;
    bool panel_on = true;
    bool low_latency = false;
    for (;;) {
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(500));
        xSemaphoreTake(fb_lock, portMAX_DELAY);
        size_t wlen = 0;
        if (linked && pending_len > 0) {
            uint8_t *tmp = work_pkt;
            work_pkt = pending_pkt;
            pending_pkt = tmp;
            wlen = pending_len;
            pending_len = 0;
            work_rx_us = pending_rx_us;
            pending_rx_us = 0;
        }
        xSemaphoreGive(fb_lock);
        if (wlen > 0) {
            frame_decoded_t decoded;
            if (frame_decode(work_pkt, wlen, &decoded)) {
                taskYIELD();
                xSemaphoreTake(fb_lock, portMAX_DELAY);
                if (linked) {
                    frame_commit(fb, &decoded);
                    frame_union(&dirty_bounds, decoded.rect);
                    atomic_store_explicit(&telemetry_io.last_frame_rev, decoded.rect.rev_lo, memory_order_relaxed);
                    if (first_frame) {
                        first_frame = false;
                        ESP_LOGI(TAG, "Applied edge frame 0x03 %ux%u (codec %u)", decoded.rect.w, decoded.rect.h, work_pkt[9]);
                    }
                }
                xSemaphoreGive(fb_lock);
            } else {
                ESP_LOGD(TAG, "Ignored invalid display frame");
            }
            taskYIELD();
        }
        xSemaphoreTake(fb_lock, portMAX_DELAY);
        if (display_idle_dimmed(linked, now_ms(), atomic_load(&last_touch), DISPLAY_IDLE_DIM_MS)) {
            atomic_store(&brightness, DIM);
        }
        int level = atomic_load(&brightness);
        if (BOARD_HAS_BATTERY && level == DIM) {
            xSemaphoreGive(fb_lock);
            if (applied != 0 && bsp_display_backlight_off() == ESP_OK) applied = 0;
            if (panel_on && esp_lcd_panel_disp_on_off(panel, false) == ESP_OK) {
                panel_on = false;
            }
            if (atomic_exchange(&ps_dirty, false) || low_latency) {
                if (wifi_set_low_latency(false)) {
                    low_latency = false;
                } else {
                    atomic_store(&ps_dirty, true);
                }
            }
            continue;
        }
        frame_bounds_t area = dirty_bounds;
        unsigned rev = atomic_load_explicit(&telemetry_io.last_frame_rev, memory_order_relaxed);
        dirty_bounds = (frame_bounds_t){ 0 };
        xSemaphoreGive(fb_lock);
        if (area.x0 < area.x1 && blit(area) == ESP_OK) {
            if (work_rx_us) {
                atomic_store_explicit(&telemetry_io.frame_flush_us,
                                      (unsigned)(esp_timer_get_time() - work_rx_us), memory_order_relaxed);
                work_rx_us = 0;
            }
            xSemaphoreTake(fb_lock, portMAX_DELAY);
            if (linked && dirty_bounds.x0 == dirty_bounds.x1) atomic_store(&shown_rev, rev);
            xSemaphoreGive(fb_lock);
        }
        if (!panel_on && esp_lcd_panel_disp_on_off(panel, true) == ESP_OK) {
            panel_on = true;
        }
        if (atomic_exchange(&ps_dirty, false) || !low_latency) {
            if (wifi_set_low_latency(true)) {
                low_latency = true;
            } else {
                atomic_store(&ps_dirty, true);
            }
        }
        if (panel_on && level != applied && bsp_display_brightness_set(level) == ESP_OK) applied = level;
    }
}

void display_set_link(bool connected)
{
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    linked = connected;
    first_frame = connected;
    pending_len = 0;
    atomic_store(&shown_rev, 0);
    if (!connected) {
        display_show_offline();
        dirty_bounds = (frame_bounds_t){ 0, 0, FRAME_WIDTH, FRAME_HEIGHT };
    }
    xSemaphoreGive(fb_lock);
    if (!connected) xTaskNotifyGive(painter);
}

bool display_apply_frame(const uint8_t *data, size_t len)
{
    if (!data || len < FRAME_HEADER_SIZE || len > FRAME_PACKET_CAP || data[0] != 0x03) return false;
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    bool applied = linked;
    if (applied) {
        memcpy(pending_pkt, data, len);
        pending_len = len;
        pending_rx_us = esp_timer_get_time();
    }
    xSemaphoreGive(fb_lock);
    if (applied && !(BOARD_HAS_BATTERY && display_is_sleeping())) xTaskNotifyGive(painter);
    return applied;
}

void display_set_brightness(int level)
{
    // The edge raises the pairing screen to 100%; DIM/BRIGHT stay the normal
    // UI range that idle sleep and touch wake restore.
    if (level < 0 || level > 100) return;
    atomic_store(&brightness, level);
    if (painter) xTaskNotifyGive(painter);
}

void display_sync_power_save(void)
{
    atomic_store(&ps_dirty, true);
    if (painter) xTaskNotifyGive(painter);
}

void display_wake(void)
{
    atomic_store(&last_touch, now_ms());
    if (!fb_lock) return;
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    bool host_controls = linked;
    xSemaphoreGive(fb_lock);
    // While linked, the edge owns brightness (the pairing screen stays at
    // 100%); the local fallback only restores dimmed offline panels.
    if (!host_controls && atomic_load(&brightness) == DIM) display_set_brightness(BRIGHT);
}

bool display_is_sleeping(void)
{
    return atomic_load(&brightness) == DIM;
}

bool display_inject_tap(uint16_t x, uint16_t y)
{
    if (x >= FRAME_WIDTH || y >= FRAME_HEIGHT || !touch_handle) return false;
    uint32_t packed = (1u << 31) | ((uint32_t)x << 16) | (uint32_t)y;
    atomic_store_explicit(&inject_tap, packed, memory_order_release);
    xTaskNotifyGive(touch_handle);
    return true;
}

void display_copy_fb_chunk(uint16_t *dst, size_t px_off, size_t px_count)
{
    if (!dst || !fb || px_off >= FRAME_PIXELS) return;
    if (px_off + px_count > FRAME_PIXELS) px_count = FRAME_PIXELS - px_off;
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    memcpy(dst, fb + px_off, px_count * sizeof(*dst));
    xSemaphoreGive(fb_lock);
}

#if BOARD_HAS_TOUCH_IRQ
static void touch_interrupt(esp_lcd_touch_handle_t controller)
{
    BaseType_t awaken = pdFALSE;
    vTaskNotifyGiveFromISR(controller->config.user_data, &awaken);
    if (awaken) portYIELD_FROM_ISR();
}
#endif

static void touch_task(void *arg)
{
    touch_gesture_t gesture = { 0 };
    uint16_t press_rev = 0;
    for (;;) {
#if BOARD_HAS_TOUCH_IRQ
        if (atomic_load_explicit(&touch_irq_ok, memory_order_relaxed)) {
            ulTaskNotifyTake(pdTRUE, gesture.down ? pdMS_TO_TICKS(12) : portMAX_DELAY);
        } else {
            vTaskDelay(pdMS_TO_TICKS(gesture.down ? 12 : 30));
        }
#else
        vTaskDelay(pdMS_TO_TICKS(gesture.down ? 12 : 30));
#endif
        uint32_t inj = atomic_exchange_explicit(&inject_tap, 0, memory_order_acq_rel);
        if (inj & (1u << 31)) {
            uint16_t ix = (uint16_t)((inj >> 16) & 0x7fff);
            uint16_t iy = (uint16_t)(inj & 0xffff);
            touch_event_t ev = touch_gesture_update(&gesture, true, ix, iy, now_ms());
            if (ev.kind == TOUCH_PRESS) {
                press_rev = atomic_load(&shown_rev);
                display_wake();
            }
            vTaskDelay(pdMS_TO_TICKS(30));
            for (int r = 0; r < 2; ++r) {
                ev = touch_gesture_update(&gesture, false, 0, 0, now_ms());
                if (ev.kind == TOUCH_TAP) {
                    net_client_pointer(LARK_PTR_TAP, ev.x, ev.y, press_rev);
                }
            }
            continue;
        }
        esp_lcd_touch_point_data_t p = { 0 };
        uint8_t count = 0;
        if (esp_lcd_touch_read_data(touch) != ESP_OK ||
            esp_lcd_touch_get_data(touch, &p, &count, 1) != ESP_OK) continue;
        bool pressed = count && p.x < FRAME_WIDTH && p.y < FRAME_HEIGHT;
        touch_event_t event = touch_gesture_update(&gesture, pressed, p.x, p.y, now_ms());
        switch (event.kind) {
        case TOUCH_PRESS: press_rev = atomic_load(&shown_rev); display_wake(); break;
        case TOUCH_TAP: net_client_pointer(LARK_PTR_TAP, event.x, event.y, press_rev); break;
        case TOUCH_DRAG: net_client_pointer(LARK_PTR_DRAG, event.x, event.y, atomic_load(&shown_rev)); break;
        case TOUCH_UP: net_client_pointer(LARK_PTR_UP, event.x, event.y, atomic_load(&shown_rev)); break;
        case TOUCH_NONE: break;
        }
    }
}

esp_err_t display_init(void)
{
    fb = heap_caps_malloc(FRAME_PIXELS * sizeof(*fb), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    pending_pkt = heap_caps_malloc(FRAME_PACKET_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    work_pkt = heap_caps_malloc(FRAME_PACKET_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    strip[0] = heap_caps_malloc(STRIP_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_DMA);
    strip[1] = heap_caps_malloc(STRIP_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_DMA);
    drawn = xSemaphoreCreateBinary();
    fb_lock = xSemaphoreCreateMutex();
    ESP_RETURN_ON_FALSE(fb && pending_pkt && work_pkt && strip[0] && strip[1] && drawn && fb_lock, ESP_ERR_NO_MEM, TAG, "Display buffers");
    bsp_display_config_t config = { .max_transfer_sz = STRIP_BYTES };
    esp_lcd_panel_io_handle_t io;
    ESP_RETURN_ON_ERROR(bsp_display_new(&config, &panel, &io), TAG, "Init LCD");
    const esp_lcd_panel_io_callbacks_t callbacks = { .on_color_trans_done = color_done };
    ESP_RETURN_ON_ERROR(esp_lcd_panel_io_register_event_callbacks(io, &callbacks, drawn), TAG, "LCD DMA callback");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(panel, true), TAG, "Enable LCD");
    // BOX-3's BSP sets the panel to landscape and already aligns GT911 coordinates.
    ESP_RETURN_ON_ERROR(bsp_touch_new(NULL, &touch), TAG, "Init " BOARD_TOUCH);
    display_show_offline();
    ESP_RETURN_ON_ERROR(blit((frame_bounds_t){ 0, 0, FRAME_WIDTH, FRAME_HEIGHT }), TAG, "First frame");
    ESP_RETURN_ON_ERROR(bsp_display_backlight_on(), TAG, "Enable backlight");
    ESP_RETURN_ON_ERROR(bsp_display_brightness_set(BRIGHT), TAG, "Set initial brightness");
    atomic_store(&last_touch, now_ms());
    TaskHandle_t touch_reader;
    ESP_RETURN_ON_FALSE(xTaskCreate(paint_task, "painter", 4096, NULL, 2, &painter) == pdPASS &&
                        xTaskCreate(touch_task, "touch", 3072, NULL, 4, &touch_reader) == pdPASS,
                        ESP_ERR_NO_MEM, TAG, "Display tasks");
    touch_handle = touch_reader;
#if BOARD_HAS_TOUCH_IRQ
    if (esp_lcd_touch_register_interrupt_callback_with_data(touch, touch_interrupt, touch_reader) == ESP_OK) {
        atomic_store_explicit(&touch_irq_ok, true, memory_order_relaxed);
        xTaskNotifyGive(touch_reader);
    } else {
        ESP_LOGW(TAG, "Touch interrupt unavailable; using poll wake");
    }
#endif
    ESP_LOGI(TAG, BOARD_LCD " %dx%d LCD and " BOARD_TOUCH " touch initialized", FRAME_WIDTH, FRAME_HEIGHT);
    return ESP_OK;
}
