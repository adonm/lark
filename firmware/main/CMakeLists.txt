idf_component_register(
    SRCS "main.c" "wifi.c" "display.c" "gfx.c" "frame.c" "audio.c" "vad.c" "touch_gesture.c"
         "power.c" "telemetry.c" "media.c" "echo_suppress.c" "net_client.c" "ota_server.c" "wake_word.c"
    INCLUDE_DIRS "."
    PRIV_REQUIRES app_update board_bsp esp_app_format esp_event esp_http_client esp_http_server
                  esp_https_ota esp_lcd esp_netif esp_timer esp_wifi lwip mbedtls nvs_flash
)
# Ship one model inside the OTA app so deployed partition tables need no change.
idf_component_get_property(sr_dir espressif__esp-sr COMPONENT_DIR)
idf_build_get_property(python PYTHON)
set(model wn9_hiesp)
set(model_source "${sr_dir}/model/wakenet_model/${model}")
set(model_stage "${CMAKE_CURRENT_BINARY_DIR}/wake")
set(model_bin "${model_stage}/wake_model.bin")
file(GLOB model_files CONFIGURE_DEPENDS "${model_source}/*")
add_custom_command(OUTPUT "${model_bin}"
    COMMAND ${CMAKE_COMMAND} -E copy_directory "${model_source}" "${model_stage}/${model}"
    COMMAND "${python}" "${sr_dir}/model/pack_model.py" -m "${model_stage}" -o wake_model.bin
    DEPENDS "${sr_dir}/model/pack_model.py" ${model_files}
    VERBATIM)
target_add_binary_data(${COMPONENT_LIB} "${model_bin}" BINARY)

target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Werror=return-type -Wno-unused-parameter)
idf_build_get_property(project_name PROJECT_NAME)
if(project_name STREQUAL "lark_box3")
    target_compile_definitions(${COMPONENT_LIB} PRIVATE LARK_BOX3=1)
endif()
