package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
	"github.com/leoleovich/3djuggler/juggler"
)

// testEndpoint builds an InternEndpoint pointed at the given base URL.
func testEndpoint(uri string) *InternEndpoint {
	return &InternEndpoint{
		APIApp:      "test-app",
		APIKey:      "test-key",
		APIURI:      uri,
		PrinterName: "North",
		OfficeName:  "Dublin DBC",
		job:         &juggler.Job{},
	}
}

// speedUpRetries shrinks the retry backoff so tests do not sleep for seconds.
func speedUpRetries(t *testing.T) {
	t.Helper()
	orig := retryInterval
	retryInterval = time.Millisecond
	t.Cleanup(func() { retryInterval = orig })
}

// captured records what a stub intern endpoint received.
type captured struct {
	mu     sync.Mutex
	forms  []url.Values
	bodies []string
	paths  []string
}

func (c *captured) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, string(body))
	c.forms = append(c.forms, form)
	c.paths = append(c.paths, r.URL.Path)
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.forms)
}

func (c *captured) last() url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.forms) == 0 {
		return nil
	}
	return c.forms[len(c.forms)-1]
}

func (c *captured) bodyAt(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bodies[i]
}

func TestReportStatSendsExpectedFields(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.reportStat(); err != nil {
		t.Fatalf("reportStat() error: %v", err)
	}

	form := rec.last()
	checks := map[string]string{
		"app":          "test-app",
		"token":        "test-key",
		"action":       "heartbeat",
		"printer_name": "North",
		"office_name":  "Dublin DBC",
	}
	for k, want := range checks {
		if got := form.Get(k); got != want {
			t.Errorf("field %q = %q, want %q", k, got, want)
		}
	}
	if rec.paths[0] != "/printer/" {
		t.Errorf("path = %q, want /printer/", rec.paths[0])
	}
}

// TestRetriesResendBody is the regression test for the retry bug: an
// http.Request body is consumed on the first attempt, so replaying the same
// request sent an empty body on every subsequent try.
func TestRetriesResendBody(t *testing.T) {
	speedUpRetries(t)

	rec := &captured{}
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		// Fail the first attempt at the transport level by hijacking and
		// closing the connection without a response.
		if atomic.AddInt32(&attempts, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.reportStat(); err != nil {
		t.Fatalf("reportStat() error: %v", err)
	}

	if rec.count() < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", rec.count())
	}
	// Every attempt must carry the full form, not just the first.
	for i := 0; i < rec.count(); i++ {
		body := rec.bodyAt(i)
		if body == "" {
			t.Errorf("attempt %d sent an empty body", i+1)
		}
		if !strings.Contains(body, "action=heartbeat") {
			t.Errorf("attempt %d body missing action: %q", i+1, body)
		}
	}
}

func TestRetriesGiveUpAfterMax(t *testing.T) {
	speedUpRetries(t)

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
				return
			}
		}
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	err := ie.reportStat()
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got := atomic.LoadInt32(&attempts); got != maxHTTPRetries {
		t.Errorf("attempts = %d, want %d", got, maxHTTPRetries)
	}
}

// TestNonOKStatusIsReported covers the calls that previously ignored the
// HTTP status code entirely.
func TestNonOKStatusIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)

	tests := map[string]func() error{
		"reportStat": ie.reportStat,
		"reschedule": ie.reschedule,
		"deleteJob":  func() error { return ie.deleteJob(&juggler.Job{ID: 1}) },
		"reportJob":  func() error { return ie.reportJobStatusChange(&juggler.Job{ID: 1, Status: juggler.StatusPrinting}) },
		"getJob":     func() error { return ie.getJob(1) },
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Errorf("%s: expected an error on HTTP 500", name)
			}
		})
	}
}

func TestGetJobDecodesJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Success":true,"Content":{"id":42,"file_name":"bracket.gcode","owner":"leoleovich","status":"New","color":"Red"}}`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.getJob(42); err != nil {
		t.Fatalf("getJob() error: %v", err)
	}
	if ie.job.ID != 42 {
		t.Errorf("job ID = %d, want 42", ie.job.ID)
	}
	if ie.job.Filename != "bracket.gcode" {
		t.Errorf("filename = %q, want bracket.gcode", ie.job.Filename)
	}
	if ie.job.Owner != "leoleovich" {
		t.Errorf("owner = %q, want leoleovich", ie.job.Owner)
	}
}

func TestGetJobEmptyQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Success":true,"Content":{"id":0}}`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	err := ie.getJob(0)
	if !errors.Is(err, ErrNothingToPrint) {
		t.Errorf("error = %v, want ErrNothingToPrint", err)
	}
}

// TestGetJobNilContent is the regression test for a nil pointer dereference:
// a successful response with a null Content used to be assigned straight to
// ie.job, so the next field access panicked.
func TestGetJobNilContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Success":true,"Content":null}`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	err := ie.getJob(0)
	if !errors.Is(err, ErrNothingToPrint) {
		t.Errorf("error = %v, want ErrNothingToPrint", err)
	}
	if ie.job == nil {
		t.Error("previous job must not be replaced with nil")
	}
}

func TestGetJobUnsuccessful(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"Success":false,"Error":"printer not found"}`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	err := ie.getJob(7)
	if err == nil {
		t.Fatal("expected an error when Success is false")
	}
	if !strings.Contains(err.Error(), "printer not found") {
		t.Errorf("error = %v, want it to include the server message", err)
	}
}

func TestGetJobMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{not json at all`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.getJob(1); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestNextJobOmitsID(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		fmt.Fprint(w, `{"Success":true,"Content":{"id":5}}`)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.nextJob(); err != nil {
		t.Fatalf("nextJob() error: %v", err)
	}
	if got := rec.last().Get("id"); got != "" {
		t.Errorf("nextJob should not send an id, got %q", got)
	}
	if got := rec.last().Get("action"); got != "get" {
		t.Errorf("action = %q, want get", got)
	}
}

func TestReportJobStatusChangeSkipsWaitingJob(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	job := &juggler.Job{ID: 1, Status: juggler.StatusWaitingJob}
	if err := ie.reportJobStatusChange(job); err != nil {
		t.Fatalf("reportJobStatusChange() error: %v", err)
	}
	if rec.count() != 0 {
		t.Errorf("expected no request for the default status, got %d", rec.count())
	}
}

// TestStatusForIntern locks down the exact strings the intern endpoint
// displays. These are part of the wire contract and must not drift.
func TestStatusForIntern(t *testing.T) {
	tests := []struct {
		name string
		job  *juggler.Job
		want string
	}{
		{
			name: "printing with progress",
			job:  &juggler.Job{Status: juggler.StatusPrinting, FeederStatus: gcodefeeder.Printing, Progress: 42.5},
			want: "Printing... (42.5%)",
		},
		{
			name: "printing rounds to one decimal",
			job:  &juggler.Job{Status: juggler.StatusPrinting, FeederStatus: gcodefeeder.Printing, Progress: 7},
			want: "Printing... (7.0%)",
		},
		{
			name: "mmu paused",
			job:  &juggler.Job{Status: juggler.StatusPaused, FeederStatus: gcodefeeder.MMUBusy},
			want: "Printing paused: MMU paused printing",
		},
		{
			name: "filament sensor paused",
			job:  &juggler.Job{Status: juggler.StatusPaused, FeederStatus: gcodefeeder.FSensorBusy},
			want: "Printing paused: Filament sensor paused printing",
		},
		{
			name: "manually paused",
			job:  &juggler.Job{Status: juggler.StatusPaused, FeederStatus: gcodefeeder.ManuallyPaused},
			want: "Printing paused manually",
		},
		{
			name: "plain status passes through",
			job:  &juggler.Job{Status: juggler.StatusWaitingButton},
			want: "Waiting for a button",
		},
		{
			name: "printing without feeder printing",
			job:  &juggler.Job{Status: juggler.StatusPrinting, FeederStatus: gcodefeeder.Ready},
			want: "Printing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusForIntern(tt.job); got != tt.want {
				t.Errorf("statusForIntern() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReportJobStatusChangeSendsProgress(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	job := &juggler.Job{ID: 9, Status: juggler.StatusPrinting, FeederStatus: gcodefeeder.Printing, Progress: 33.3}
	if err := ie.reportJobStatusChange(job); err != nil {
		t.Fatalf("reportJobStatusChange() error: %v", err)
	}

	form := rec.last()
	if got := form.Get("status"); got != "Printing... (33.3%)" {
		t.Errorf("status = %q", got)
	}
	if got := form.Get("id"); got != "9" {
		t.Errorf("id = %q, want 9", got)
	}
	if got := form.Get("action"); got != "update" {
		t.Errorf("action = %q, want update", got)
	}
}

func TestDeleteJobSendsID(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.deleteJob(&juggler.Job{ID: 123}); err != nil {
		t.Fatalf("deleteJob() error: %v", err)
	}
	form := rec.last()
	if got := form.Get("action"); got != "delete" {
		t.Errorf("action = %q, want delete", got)
	}
	if got := form.Get("id"); got != "123" {
		t.Errorf("id = %q, want 123", got)
	}
}

func TestRescheduleHitsPrinterPath(t *testing.T) {
	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.reschedule(); err != nil {
		t.Fatalf("reschedule() error: %v", err)
	}
	if rec.paths[0] != "/printer/" {
		t.Errorf("path = %q, want /printer/", rec.paths[0])
	}
	if got := rec.last().Get("action"); got != "reschedule" {
		t.Errorf("action = %q, want reschedule", got)
	}
}

func TestPostSetsFormContentType(t *testing.T) {
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ie := testEndpoint(srv.URL)
	if err := ie.reportStat(); err != nil {
		t.Fatalf("reportStat() error: %v", err)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", contentType)
	}
}
