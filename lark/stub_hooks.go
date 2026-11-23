package main

import (
	"context"
)

// stubConfig is non-nil only for e2e builds that set LARK_STUB=1. The normal
// binary never links a scripted STT, TTS engine or Delta Chat backend.
type stubConfig struct {
	stt        func(context.Context, []byte) (string, error)
	openEngine func() (speechEngine, error)
	endpoint   *endpointSettings
	startDC    func(*larkServer)
}
