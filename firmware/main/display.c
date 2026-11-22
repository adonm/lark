#include "display.h"

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

enum { STRIP_ROWS = 24, STRIP_BYTES = STRIP_ROWS * FRAME_WIDTH * sizeof(uint16_t), BRIGHT = 85, DIM = 12 };
_Static_assert(BSP_LCD_H_RES == FRAME_WIDTH && BSP_LCD_V_RES == FRAME_HEIGHT && BSP_LCD_BIGENDIAN,
               "Framebuffer must match the LCD geometry and byte order");
static const char *TAG = "display";
static esp_lcd_panel_handle_t panel;
static esp_lcd_touch_handle_t touch;
static SemaphoreHandle_t drawn, fb_lock;
static TaskHandle_t painter;
static uint16_t *fb, *strip[2];
static uint8_t *pending_pkt, *work_pkt;
static size_t pending_len;
static bool linked;
static bool first_frame;
static frame_bounds_t dirty_bounds;
static atomic_int brightness = BRIGHT;
static atomic_uint last_touch;
static atomic_uint shown_rev;
static atomic_bool ps_dirty = true;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

static bool color_done(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *event, void *ctx)
{
    BaseType_t awaken = pdFALSE;
    xSemaphoreGiveFromISR(ctx, &awaken);
    return awaken == pdTRUE;
}

static void copy_strip(uint16_t *dst, frame_bounds_t area, int y, int rows)
{
    int width = area.x1 - area.x0;
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    if (width == FRAME_WIDTH) {
        memcpy(dst, fb + y * FRAME_WIDTH, (size_t)rows * FRAME_WIDTH * sizeof(*dst));
    } else {
        for (int row = 0; row < rows; ++row) {
            memcpy(dst + row * width, fb + (y + row) * FRAME_WIDTH + area.x0, (size_t)width * sizeof(*dst));
        }
    }
    xSemaphoreGive(fb_lock);
}

static esp_err_t blit(frame_bounds_t area)
{
    if (area.x0 >= area.x1 || area.y0 >= area.y1) return ESP_OK;
    int64_t started = esp_timer_get_time();
    int cur = 0;
    bool in_flight = false;
    esp_err_t err = ESP_OK;
    for (int y = area.y0; y < area.y1; y += STRIP_ROWS) {
        int rows = area.y1 - y < STRIP_ROWS ? area.y1 - y : STRIP_ROWS;
        // Copy the next strip into strip[cur] while strip[cur ^ 1] transfers over SPI DMA.
        copy_strip(strip[cur], area, y, rows);
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
    }
    if (in_flight) {
        xSemaphoreTake(drawn, portMAX_DELAY);
    }
    atomic_store_explicit(&telemetry_io.display_flush_ms, (esp_timer_get_time() - started) / 1000, memory_order_relaxed);
    return err;
}

static void display_show_offline(void)
{
    gfx_offline_cat(fb);
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
                        ESP_LOGI(TAG, "Applied bridge frame 0x03 %ux%u (codec %u)", decoded.rect.w, decoded.rect.h, work_pkt[9]);
                    }
                }
                xSemaphoreGive(fb_lock);
            } else {
                ESP_LOGD(TAG, "Ignored invalid display frame");
            }
            taskYIELD();
        }
        xSemaphoreTake(fb_lock, portMAX_DELAY);
        if (!linked && now_ms() - atomic_load(&last_touch) >= 120000u) {
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
    }
    xSemaphoreGive(fb_lock);
    if (applied && !(BOARD_HAS_BATTERY && display_is_sleeping())) xTaskNotifyGive(painter);
    return applied;
}

void display_set_brightness(int level)
{
    if (level < DIM || level > BRIGHT) return;
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
    if (atomic_load(&brightness) != BRIGHT) display_set_brightness(BRIGHT);
}

bool display_is_sleeping(void)
{
    return atomic_load(&brightness) == DIM;
}

#if BOARD_HAS_BATTERY
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
#if BOARD_HAS_BATTERY
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(display_is_sleeping() && !gesture.down ? 120 : 30));
#else
        vTaskDelay(pdMS_TO_TICKS(30));
#endif
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
#if BOARD_HAS_BATTERY
    if (esp_lcd_touch_register_interrupt_callback_with_data(touch, touch_interrupt, touch_reader) != ESP_OK) {
        ESP_LOGW(TAG, "Touch interrupt unavailable; using poll wake");
    }
#endif
    ESP_LOGI(TAG, BOARD_LCD " %dx%d LCD and " BOARD_TOUCH " touch initialized", FRAME_WIDTH, FRAME_HEIGHT);
    return ESP_OK;
}
