cmake_minimum_required(VERSION 3.16)

set(firmware "${CMAKE_CURRENT_LIST_DIR}/../..")
set(PROJECT_VER "0.13.0")
set(EXTRA_COMPONENT_DIRS "${firmware}/main")
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(lark_box3)
