//go:build linux && parakeet

package main

/*
#cgo LDFLAGS: -ltranscribe -lggml -lggml-cpu -lggml-vulkan -lggml-base -lstdc++ -lm -lpthread -ldl -lvulkan
#include <stdlib.h>
#include <transcribe.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"unsafe"
)

type parakeetSTT struct {
	model *C.struct_transcribe_model
	slot  chan struct{}
}

func openParakeet(path string) (*parakeetSTT, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var params C.struct_transcribe_model_load_params
	C.transcribe_model_load_params_init(&params)
	params.backend = C.TRANSCRIBE_BACKEND_VULKAN
	var model *C.struct_transcribe_model
	if err := parakeetError(C.transcribe_model_load_file(cpath, &params, &model)); err != nil {
		return nil, err
	}
	var info C.struct_transcribe_device_info
	C.transcribe_device_info_init(&info)
	status := C.transcribe_device_get_info(C.transcribe_model_device(model), &info)
	if status != C.TRANSCRIBE_OK || (info.device_type != C.TRANSCRIBE_DEVICE_TYPE_GPU && info.device_type != C.TRANSCRIBE_DEVICE_TYPE_IGPU) {
		C.transcribe_model_free(model)
		return nil, errors.New("Parakeet requires a Vulkan GPU")
	}
	log.Printf("Parakeet Vulkan selected %s (%s)", C.GoString(info.description), C.GoString(info.name))
	return &parakeetSTT{model: model, slot: make(chan struct{}, 1)}, nil
}

func (p *parakeetSTT) close() {
	p.slot <- struct{}{}
	defer func() { <-p.slot }()
	C.transcribe_model_free(p.model)
	p.model = nil
}

func (p *parakeetSTT) transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.model == nil {
		return "", errors.New("Parakeet is closed")
	}
	var params C.struct_transcribe_session_params
	C.transcribe_session_params_init(&params)
	params.n_threads = 1 // The Vulkan encoder runs on the GPU; the decoder runs here.
	var session *C.struct_transcribe_session
	if err := parakeetError(C.transcribe_session_init(p.model, &params, &session)); err != nil {
		return "", err
	}
	defer C.transcribe_session_free(session)
	samples := parakeetSamples(pcm)
	var run C.struct_transcribe_run_params
	C.transcribe_run_params_init(&run)
	run.timestamps = C.TRANSCRIBE_TIMESTAMPS_SEGMENT
	status := C.transcribe_run(session, (*C.float)(unsafe.Pointer(&samples[0])), C.int(len(samples)), &run)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text := strings.TrimSpace(C.GoString(C.transcribe_full_text(session)))
	if status != C.TRANSCRIBE_OK && (status != C.TRANSCRIBE_ERR_OUTPUT_REPETITION || text == "") {
		return "", parakeetError(status)
	}
	return cleanParakeetText(text), nil
}

func parakeetError(status C.transcribe_status) error {
	if status == C.TRANSCRIBE_OK {
		return nil
	}
	return fmt.Errorf("Parakeet: %s", C.GoString(C.transcribe_status_string(C.int(status))))
}
