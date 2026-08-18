package main

import (
	"io"
	"os"
	"runtime/debug"
	"testing"

	log "github.com/sirupsen/logrus"
)

// TestMain silences the daemon's logging so test output shows test results
// rather than thousands of handler log lines.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func TestVersionFromSettings(t *testing.T) {
	const full = "2d3503b7df25000000000000000000000000000a"

	tests := []struct {
		name     string
		settings []debug.BuildSetting
		want     string
	}{
		{
			name:     "revision is truncated to twelve characters",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: full}},
			want:     "2d3503b7df25",
		},
		{
			name: "a modified tree is marked dirty",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: full},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "2d3503b7df25-dirty",
		},
		{
			name: "an unmodified tree is not marked dirty",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: full},
				{Key: "vcs.modified", Value: "false"},
			},
			want: "2d3503b7df25",
		},
		{
			name:     "a revision shorter than the limit is left alone",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			want:     "abc123",
		},
		{
			name:     "no settings at all",
			settings: nil,
			want:     "unknown",
		},
		{
			name:     "settings without a revision",
			settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
			want:     "unknown",
		},
		{
			// Guards against the key being misspelled or renamed: the value
			// is present but under a key we do not read.
			name:     "revision under an unrecognised key is ignored",
			settings: []debug.BuildSetting{{Key: "vcs.rev", Value: full}},
			want:     "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionFromSettings(tt.settings); got != tt.want {
				t.Errorf("versionFromSettings() = %q, want %q", got, tt.want)
			}
		})
	}
}
