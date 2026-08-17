package juggler

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestJobJSONFieldNames locks the wire format. These keys are consumed by the
// intern frontend, by gizmosync and by anyone curling /info, so a rename here
// is a breaking change.
func TestJobJSONFieldNames(t *testing.T) {
	job := Job{
		ID:          1,
		Filename:    "part.gcode",
		Owner:       "someone",
		Status:      StatusPrinting,
		Color:       "Red",
		Progress:    12.5,
		Fetched:     time.Unix(1700000000, 0).UTC(),
		Scheduled:   time.Unix(1700000600, 0).UTC(),
		PrinterName: "North",
	}

	b, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	want := []string{"id", "file_name", "owner", "status", "color", "progress", "fetched", "scheduled", "printer_name"}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON key %q in %s", key, b)
		}
	}

	// FeederStatus is internal and must never be serialised.
	if _, ok := raw["FeederStatus"]; ok {
		t.Error("FeederStatus must not appear in JSON")
	}
}

// TestJobOmitsEmptyFileContent ensures /info does not ship the gcode body.
func TestJobOmitsEmptyFileContent(t *testing.T) {
	b, err := json.Marshal(Job{ID: 1})
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if strings.Contains(string(b), "file_content") {
		t.Errorf("empty file_content must be omitted, got %s", b)
	}
}

// TestJobRoundTrip covers decoding an intern endpoint response.
func TestJobRoundTrip(t *testing.T) {
	input := `{
		"id": 42,
		"file_name": "bracket.gcode",
		"file_content": "G28\nG1 X10\n",
		"owner": "leoleovich",
		"status": "Printing",
		"color": "Galaxy Black",
		"progress": 55.5,
		"printer_name": "North"
	}`

	var job Job
	if err := json.Unmarshal([]byte(input), &job); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if job.ID != 42 {
		t.Errorf("ID = %d, want 42", job.ID)
	}
	if job.Filename != "bracket.gcode" {
		t.Errorf("Filename = %q", job.Filename)
	}
	if job.FileContent != "G28\nG1 X10\n" {
		t.Errorf("FileContent = %q", job.FileContent)
	}
	if job.Status != StatusPrinting {
		t.Errorf("Status = %q, want %q", job.Status, StatusPrinting)
	}
	if job.Color != "Galaxy Black" {
		t.Errorf("Color = %q", job.Color)
	}
	if job.Progress != 55.5 {
		t.Errorf("Progress = %v, want 55.5", job.Progress)
	}
}

// TestJobStatusValues pins the exact status strings. The intern backend
// matches on these, so they are part of the contract.
func TestJobStatusValues(t *testing.T) {
	tests := map[JobStatus]string{
		StatusWaitingJob:    "Waiting for job",
		StatusWaitingButton: "Waiting for a button",
		StatusPrinting:      "Printing",
		StatusSending:       "Sending to printer",
		StatusCancelling:    "Cancelling",
		StatusFinished:      "Finished",
		StatusButtonTimeout: "Button timeout",
		StatusPaused:        "Paused",
	}
	for status, want := range tests {
		if string(status) != want {
			t.Errorf("status = %q, want %q", string(status), want)
		}
	}
}

func TestSetHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	SetHeaders(w)

	tests := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Headers": "Content-Type",
		"Access-Control-Allow-Methods": "GET",
	}
	for header, want := range tests {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// Not every endpoint returns JSON, so the base helper must not claim it.
	if got := w.Header().Get("Content-Type"); got != "" {
		t.Errorf("SetHeaders should not set Content-Type, got %q", got)
	}
}

func TestSetJSONHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	SetJSONHeaders(w)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}
