cmake_minimum_required(VERSION 3.16)

set(PROJECT_VER "0.9.5")
set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_LIST_DIR}/boards/vocat/board_bsp")
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(lark)

include(cjson.cmake)
