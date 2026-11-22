//go:build linux

package main

/*
#include <malloc.h>

__attribute__((constructor))
static void lark_limit_malloc_arenas(void) {
	mallopt(M_ARENA_MAX, 2);
}
*/
import "C"
