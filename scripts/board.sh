# shellcheck shell=bash
# Variables are consumed by the sourcing build, flash, and OTA scripts.
# shellcheck disable=SC2034
case ${BOARD:-vocat} in
    vocat)
        build_dir=build
        project_dir=.
        board_image=lark.bin
        board_name=esp-vocat
        ;;
    box3)
        build_dir=build-box3
        project_dir=boards/box3
        board_image=lark_box3.bin
        board_name=esp-box-3
        ;;
    *) echo "Unknown BOARD: $BOARD (expected vocat or box3)" >&2; exit 2 ;;
esac
