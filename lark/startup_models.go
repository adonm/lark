package main

import (
	"context"
	"log"
	"runtime"
	"sync"
	"time"
)

// modelPaths are resolved once at startup; the files themselves are checked
// and loaded by loadModels after the HTTP listener is up.
type modelPaths struct {
	parakeet  string
	vad       string
	smartTurn string
}

// loadModels starts Parakeet, Silero and Smart Turn in the background. The
// returned channel closes after every load has finished (or shut down).
func (b *larkServer) loadModels(ctx context.Context, paths modelPaths) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.loadVAD(ctx, paths.vad)
		}()
		go func() {
			defer wg.Done()
			b.loadParakeet(ctx, paths.parakeet)
		}()
		b.loadSmartTurn(ctx, paths.smartTurn)
		wg.Wait()
		if ctx.Err() == nil {
			log.Printf("Lark models ready in %s", time.Since(b.models.stt.started).Round(time.Millisecond))
		}
	}()
	return done
}

func (b *larkServer) loadVAD(ctx context.Context, path string) {
	if err := checkEndpointModel(path); err != nil {
		if ctx.Err() == nil {
			b.models.vad.setError(err)
			log.Printf("Lark endpoint VAD unavailable: %v", err)
		}
		return
	}
	vad := newSileroVAD(path)
	if vad == nil {
		if ctx.Err() == nil {
			b.models.vad.setError(errVADLoad)
			log.Print("Lark endpoint VAD unavailable: could not load the pinned Silero v5 model")
		}
		return
	}
	vad.close()
	b.models.vad.setReady()
	if ctx.Err() == nil {
		log.Print("Lark endpoint VAD ready (in-process Silero v5)")
	}
}

func (b *larkServer) loadParakeet(ctx context.Context, path string) {
	if err := checkParakeetModel(path); err != nil {
		if ctx.Err() == nil {
			b.models.stt.setError(err)
			log.Printf("Lark speech-to-text unavailable: %v", err)
		}
		return
	}
	parakeet, err := openParakeet(path)
	if err != nil {
		if ctx.Err() == nil {
			b.models.stt.setError(err)
			log.Printf("Lark speech-to-text unavailable: %v", err)
		}
		return
	}
	// One real transcribe validates the Vulkan backend; a load that cannot
	// decode must never be reported ready.
	if _, err := parakeet.transcribe(ctx, make([]byte, sampleRate*2)); err != nil {
		if ctx.Err() == nil {
			b.models.stt.setError(err)
			log.Printf("Lark speech-to-text unavailable: %v", err)
		}
		return
	}
	b.setSTT(parakeet.transcribe)
	b.models.stt.setReady()
	if ctx.Err() == nil {
		log.Print("Lark speech-to-text ready (Parakeet, in-process Vulkan)")
	}
	go func() {
		<-ctx.Done()
		b.setSTT(nil)
		parakeet.close()
	}()
}

func (b *larkServer) loadSmartTurn(ctx context.Context, path string) {
	smartTurn, err := openSmartTurn(path, min(4, runtime.GOMAXPROCS(0)))
	if err != nil {
		if ctx.Err() == nil {
			b.models.smartTurn.setError(err)
			log.Printf("Smart Turn disabled: %v", err)
		}
		return
	}
	b.endpoint.setSmartTurn(smartTurn.predictPCM16)
	b.models.smartTurn.setReady()
	if ctx.Err() == nil {
		log.Print("Lark semantic endpoint ready (Smart Turn v3.2)")
	}
	go func() {
		<-ctx.Done()
		b.endpoint.setSmartTurn(nil)
		smartTurn.close()
	}()
}
