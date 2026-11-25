//go:build !linux || !parakeet

package audio

import (
	"context"
	"errors"
)

type STT struct{}

var errParakeetBuild = errors.New("Parakeet requires a Linux bridge built with the parakeet tag")

func OpenParakeet(string) (*STT, error) { return nil, errParakeetBuild }
func (*STT) Close()                     {}
func (*STT) Transcribe(context.Context, []byte) (string, error) {
	return "", errParakeetBuild
}
