#pragma once

#define BOARD_HAS_TOUCH_IRQ 1

#ifdef LARK_BOX3
#define BOARD_NAME "esp-box-3"
#define BOARD_ID_PREFIX "box3-"
#define BOARD_LCD "ILI9341"
#define BOARD_TOUCH "GT911"
#define BOARD_HAS_BATTERY 0
enum { FRAME_WIDTH = 320, FRAME_HEIGHT = 240 };
#else
#define BOARD_NAME "esp-vocat"
#define BOARD_ID_PREFIX "vocat-"
#define BOARD_LCD "ST77916"
#define BOARD_TOUCH "CST816S"
#define BOARD_HAS_BATTERY 1
enum { FRAME_WIDTH = 360, FRAME_HEIGHT = 360 };
#endif
