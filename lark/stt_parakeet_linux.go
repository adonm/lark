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
	"time"
	"unsafe"
)

type sttResult struct {
	text string
	err  error
}

type sttJob struct {
	ctx    context.Context
	pcm    []byte
	at     time.Time
	timing *sttTiming
	res    chan sttResult
}

type parakeetSTT struct {
	model   *C.struct_transcribe_model
	session *C.struct_transcribe_session
	commits chan sttJob
	probes  chan sttJob
	quit    chan struct{}
	done    chan struct{}
	scratch []float32
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
	var sparams C.struct_transcribe_session_params
	C.transcribe_session_params_init(&sparams)
	sparams.n_threads = 1 // The Vulkan encoder runs on the GPU; the decoder runs here.
	var session *C.struct_transcribe_session
	if err := parakeetError(C.transcribe_session_init(model, &sparams, &session)); err != nil {
		C.transcribe_model_free(model)
		return nil, err
	}
	log.Printf("Parakeet Vulkan selected %s (%s)", C.GoString(info.description), C.GoString(info.name))
	p := &parakeetSTT{
		model:   model,
		session: session,
		commits: make(chan sttJob, 16),
		probes:  make(chan sttJob, 16),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
		scratch: make([]float32, 0, sampleRate*15),
	}
	go p.worker()
	return p, nil
}

func (p *parakeetSTT) worker() {
	defer close(p.done)
	for {
		var job sttJob
		select {
		case <-p.quit:
			return
		case job = <-p.commits:
		default:
			select {
			case <-p.quit:
				return
			case job = <-p.commits:
			case job = <-p.probes:
			}
		}
		p.runJob(job)
	}
}

func (p *parakeetSTT) runJob(job sttJob) {
	if err := job.ctx.Err(); err != nil {
		job.res <- sttResult{err: err}
		return
	}
	runStart := time.Now()
	queueDur := runStart.Sub(job.at)
	p.scratch = parakeetSamplesInto(p.scratch, job.pcm)
	if len(p.scratch) == 0 {
		job.res <- sttResult{}
		return
	}
	var run C.struct_transcribe_run_params
	C.transcribe_run_params_init(&run)
	run.timestamps = C.TRANSCRIBE_TIMESTAMPS_NONE
	status := C.transcribe_run(p.session, (*C.float)(unsafe.Pointer(&p.scratch[0])), C.int(len(p.scratch)), &run)
	if job.timing != nil {
		job.timing.queue = queueDur
		job.timing.run = time.Since(runStart)
	}
	if err := job.ctx.Err(); err != nil {
		job.res <- sttResult{err: err}
		return
	}
	text := strings.TrimSpace(C.GoString(C.transcribe_full_text(p.session)))
	if status != C.TRANSCRIBE_OK && (status != C.TRANSCRIBE_ERR_OUTPUT_REPETITION || text == "") {
		job.res <- sttResult{err: parakeetError(status)}
		return
	}
	job.res <- sttResult{text: cleanParakeetText(text)}
}

func (p *parakeetSTT) close() {
	select {
	case <-p.quit:
		return
	default:
		close(p.quit)
	}
	<-p.done
	if p.session != nil {
		C.transcribe_session_free(p.session)
		p.session = nil
	}
	if p.model != nil {
		C.transcribe_model_free(p.model)
		p.model = nil
	}
}

func (p *parakeetSTT) transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) < 2 {
		return "", nil
	}
	job := sttJob{
		ctx:    ctx,
		pcm:    pcm,
		at:     time.Now(),
		timing: sttTimingFrom(ctx),
		res:    make(chan sttResult, 1),
	}
	q := p.commits
	if isSTTProbe(ctx) {
		q = p.probes
	}
	select {
	case q <- job:
	case <-p.quit:
		return "", errors.New("Parakeet is closed")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case r := <-job.res:
		return r.text, r.err
	case <-p.quit:
		return "", errors.New("Parakeet is closed")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func parakeetError(status C.transcribe_status) error {
	if status == C.TRANSCRIBE_OK {
		return nil
	}
	return fmt.Errorf("Parakeet: %s", C.GoString(C.transcribe_status_string(C.int(status))))
}
