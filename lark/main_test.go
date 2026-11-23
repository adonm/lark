package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	debugScreenText = true
	os.Exit(m.Run())
}
