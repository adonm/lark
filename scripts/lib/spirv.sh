# shellcheck shell=bash
# SPIR-V header discovery shared by setup-host.sh and build-parakeet.sh.
find_spirv_headers() {
    local IFS=: dir rel
    for dir in ${CMAKE_PREFIX_PATH:-} /usr /usr/local \
        /home/linuxbrew/.linuxbrew/opt/spirv-headers \
        /run/host/*/*/linuxbrew/.linuxbrew/opt/spirv-headers; do
        [[ -n $dir ]] || continue
        for rel in share lib lib64; do
            if [[ -f $dir/$rel/cmake/SPIRV-Headers/SPIRV-HeadersConfig.cmake ]]; then
                printf '%s\n' "$dir"
                return 0
            fi
        done
    done
    return 1
}

# Export the prefix for CMake and, when headers live outside /usr, for the
# compiler. Returns 1 when SPIRV-Headers is missing.
use_spirv_headers() {
    local prefix
    prefix=$(find_spirv_headers) || return 1
    export CMAKE_PREFIX_PATH="$prefix${CMAKE_PREFIX_PATH:+:$CMAKE_PREFIX_PATH}"
    if [[ $prefix != /usr && -d $prefix/include ]]; then
        export CPLUS_INCLUDE_PATH="$prefix/include${CPLUS_INCLUDE_PATH:+:$CPLUS_INCLUDE_PATH}"
    fi
}
