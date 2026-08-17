package main

import (
	"io"
	"os"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestMain silences the daemon's logging so test output shows test results
// rather than thousands of handler log lines.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}
