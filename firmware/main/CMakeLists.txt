idf_component_register(
    SRCS "main.c" "wifi.c" "display.c" "display_logic.c" "gfx.c" "frame.c" "audio.c"
         "audio_logic.c" "vad.c" "touch_gesture.c" "power.c" "telemetry.c" "telemetry_logic.c" "media.c"
         "net_client.c" "net_logic.c" "ota_server.c"
    INCLUDE_DIRS "."
    PRIV_REQUIRES app_update board_bsp esp_app_format esp_event esp_http_client esp_http_server
                  esp_https_ota esp_lcd esp_netif esp_timer esp_wifi lwip mbedtls nvs_flash
)

target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Werror=return-type -Wno-unused-parameter)
idf_build_get_property(project_name PROJECT_NAME)
if(project_name STREQUAL "lark_box3")
    target_compile_definitions(${COMPONENT_LIB} PRIVATE LARK_BOX3=1)
endif()
