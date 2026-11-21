//go:build !linux || !parakeet

package main

import (
	"context"
	"errors"
)

type parakeetSTT struct{}

var errParakeetBuild = errors.New("Parakeet requires a Linux bridge built with the parakeet tag")

func openParakeet(string) (*parakeetSTT, error) { return nil, errParakeetBuild }
func (*parakeetSTT) close()                     {}
func (*parakeetSTT) transcribe(context.Context, []byte) (string, error) {
	return "", errParakeetBuild
}
