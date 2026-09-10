package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leoleovich/3djuggler/juggler"
)

// daemonTestTimeout bounds how long a test waits for the daemon to apply a
// state change. Raise it on slow CI with DAEMON_TEST_TIMEOUT_SCALE=4.
var daemonTestTimeout = func() time.Duration {
	base := 5 * time.Second
	if v := os.Getenv("DAEMON_TEST_TIMEOUT_SCALE"); v != "" {
		if scale, err := strconv.Atoi(v); err == nil && scale > 0 {
			return base * time.Duration(scale)
		}
	}
	return base
}()

// newTestDaemon builds a daemon with a drained status channel, ready for
// handler tests. Nothing polls the channel, so handlers that wait for a
// status change will time out unless the test applies it.
func newTestDaemon(status juggler.JobStatus) *Daemon {
	return &Daemon{
		config: &Config{
			Listen:         "[::1]:0",
			Serial:         "/dev/null",
			InternEndpoint: &InternEndpoint{PrinterName: "North", OfficeName: "Dublin DBC"},
		},
		job:        &juggler.Job{Status: status},
		ie:         testEndpoint("http://127.0.0.1:1"),
		statusChan: make(chan juggler.JobStatus, 10),
	}
}

// shortenHandlerTimeouts makes handler waits fast enough for tests.
func shortenHandlerTimeouts(t *testing.T) {
	t.Helper()
	origTimeout, origPoll := statusChangeTimeout, statusPollInterval
	statusChangeTimeout = 200 * time.Millisecond
	statusPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		statusChangeTimeout = origTimeout
		statusPollInterval = origPoll
	})
}

// applyStatusChanges mimics the polling loop by copying requested status
// changes onto the job until the test finishes.
func applyStatusChanges(t *testing.T, d *Daemon) {
	t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case s := <-d.statusChan:
				d.mutateJob(func(j *juggler.Job) { j.Status = s })
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		wg.Wait()
	})
}

func doRequest(d *Daemon, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	d.registerHandlers(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func TestInfoHandlerReturnsJob(t *testing.T) {
	d := newTestDaemon(juggler.StatusPrinting)
	d.mutateJob(func(j *juggler.Job) {
		j.ID = 77
		j.Owner = "leoleovich"
		j.Filename = "bracket.gcode"
		j.Progress = 44
		j.Color = "Red"
	})

	w := doRequest(d, "/info")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got juggler.Job
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.ID != 77 {
		t.Errorf("id = %d, want 77", got.ID)
	}
	if got.Owner != "leoleovich" {
		t.Errorf("owner = %q, want leoleovich", got.Owner)
	}
	if got.Status != juggler.StatusPrinting {
		t.Errorf("status = %q, want %q", got.Status, juggler.StatusPrinting)
	}
	if got.Progress != 44 {
		t.Errorf("progress = %v, want 44", got.Progress)
	}
	// PrinterName comes from config, not from the job.
	if got.PrinterName != "North" {
		t.Errorf("printer_name = %q, want North", got.PrinterName)
	}
}

// TestInfoHandlerFieldNames locks the JSON keys, which the intern frontend
// and gizmosync both parse.
func TestInfoHandlerFieldNames(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingButton)
	w := doRequest(d, "/info")

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	for _, key := range []string{"id", "file_name", "owner", "status", "color", "progress", "fetched", "scheduled", "printer_name"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing JSON field %q", key)
		}
	}
	// file_content must never be exposed over /info.
	if _, ok := raw["file_content"]; ok {
		t.Error("/info must not expose file_content")
	}
}

func TestVersionHandler(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingJob)
	w := doRequest(d, "/version")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	// The commit comes from the build, so the value depends on how the test
	// binary was produced. It must always report something.
	if got := strings.TrimSpace(w.Body.String()); got == "" {
		t.Error("/version returned an empty body")
	}
}

func TestVersionIsReported(t *testing.T) {
	if got := version(); got == "" {
		t.Error("version() returned an empty string")
	}
}

func TestStartHandlerRejectsWrongStatus(t *testing.T) {
	for _, status := range []juggler.JobStatus{
		juggler.StatusWaitingJob,
		juggler.StatusPrinting,
		juggler.StatusFinished,
		juggler.StatusCancelling,
	} {
		t.Run(string(status), func(t *testing.T) {
			d := newTestDaemon(status)
			w := doRequest(d, "/start")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestStartHandlerRequestsSending(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusWaitingButton)
	applyStatusChanges(t, d)

	w := doRequest(d, "/start")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := d.jobStatus(); got != juggler.StatusSending {
		t.Errorf("job status = %q, want %q", got, juggler.StatusSending)
	}
}

// TestStartHandlerTimesOut is the regression test for the unbounded busy-wait:
// if the polling loop never applies the status, the handler used to spin
// forever holding the connection open.
func TestStartHandlerTimesOut(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusWaitingButton)
	// Deliberately no applyStatusChanges: nothing will move the status.

	done := make(chan int, 1)
	go func() {
		done <- doRequest(d, "/start").Code
	}()

	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", code)
		}
	case <-time.After(daemonTestTimeout):
		t.Fatal("StartHandler hung instead of timing out")
	}
}

func TestStartHandlerUnpauseWithoutFeeder(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusPaused)
	// feeder is nil, which used to panic.

	w := doRequest(d, "/start")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestPauseHandlerRejectsWhenNotPrinting(t *testing.T) {
	for _, status := range []juggler.JobStatus{
		juggler.StatusWaitingJob,
		juggler.StatusWaitingButton,
		juggler.StatusPaused,
		juggler.StatusFinished,
	} {
		t.Run(string(status), func(t *testing.T) {
			d := newTestDaemon(status)
			w := doRequest(d, "/pause")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

// TestPauseHandlerWithoutFeeder covers the nil dereference: status said
// Printing but the feeder was gone.
func TestPauseHandlerWithoutFeeder(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusPrinting)

	w := doRequest(d, "/pause")
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
}

func TestRescheduleHandlerExtendsDeadline(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingButton)
	d.mutateJob(func(j *juggler.Job) { j.Scheduled = time.Now().Add(time.Minute) })
	before := d.jobSnapshot().Scheduled

	w := doRequest(d, "/reschedule")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	after := d.jobSnapshot().Scheduled
	if !after.After(before) {
		t.Errorf("scheduled time was not extended: before=%v after=%v", before, after)
	}
	// It should be roughly waitingForButtonInterval from now.
	want := time.Now().Add(waitingForButtonInterval)
	if diff := after.Sub(want); diff > 5*time.Second || diff < -5*time.Second {
		t.Errorf("scheduled = %v, want approximately %v", after, want)
	}
}

func TestRescheduleHandlerRejectsWrongStatus(t *testing.T) {
	d := newTestDaemon(juggler.StatusPrinting)
	w := doRequest(d, "/reschedule")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCancelHandlerRejectsWhenNoJob(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingJob)
	// ID is 0, meaning nothing is scheduled.
	w := doRequest(d, "/cancel")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCancelHandlerRequestsCancelling(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusPrinting)
	d.mutateJob(func(j *juggler.Job) { j.ID = 5 })
	applyStatusChanges(t, d)

	w := doRequest(d, "/cancel")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := d.jobStatus(); got != juggler.StatusCancelling {
		t.Errorf("job status = %q, want %q", got, juggler.StatusCancelling)
	}
}

// TestCancelHandlerAcceptsWaitingJob covers the race where the polling loop
// completes the cancel and moves to WaitingJob before the handler looks.
func TestCancelHandlerAcceptsWaitingJob(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusPrinting)
	d.mutateJob(func(j *juggler.Job) { j.ID = 5 })

	// Jump straight past Cancelling to WaitingJob.
	go func() {
		<-d.statusChan
		d.mutateJob(func(j *juggler.Job) { j.Status = juggler.StatusWaitingJob })
	}()

	w := doRequest(d, "/cancel")
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestUpdateStatusDoesNotBlockWhenChannelFull(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingJob)
	// Fill the buffered channel beyond capacity.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			d.UpdateStatus(juggler.StatusPrinting)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateStatus blocked when the channel was full")
	}
}

func TestHandlersSetCORSHeaders(t *testing.T) {
	d := newTestDaemon(juggler.StatusWaitingJob)
	w := doRequest(d, "/info")

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := w.Header().Get("Content-Type"); got == "" {
		t.Error("Content-Type not set")
	}
}

// TestConcurrentHandlerAccess exercises the handlers against a moving job so
// -race can catch unsynchronised reads of daemon state.
func TestConcurrentHandlerAccess(t *testing.T) {
	shortenHandlerTimeouts(t)
	d := newTestDaemon(juggler.StatusPrinting)
	d.mutateJob(func(j *juggler.Job) { j.ID = 1 })

	mux := http.NewServeMux()
	d.registerHandlers(mux)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: mutate the job continuously.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				d.mutateJob(func(j *juggler.Job) {
					j.Progress = float64(i % 100)
					j.ID = i
				})
			}
		}
	}()

	// Readers: hit /info repeatedly.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/info", nil))
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestFailedDeleteDoesNotAdvance drives the real terminal-state handler. If
// the backend delete fails, finishJob must leave the daemon where it is:
// advancing to WaitingJob lets the next poll fetch the very same job the
// backend still has queued, and print it a second time.
func TestFailedDeleteDoesNotAdvance(t *testing.T) {
	speedUpRetries(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "action=delete") {
			http.Error(w, "transient failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDaemon(juggler.StatusFinished)
	d.ie = testEndpoint(srv.URL)
	d.mutateJob(func(j *juggler.Job) { j.ID = 42 })
	d.setFeeder(nil)

	d.finishJob()

	select {
	case s := <-d.statusChan:
		t.Errorf("daemon requested %q despite a failed delete; the job can be reprinted", s)
	default:
	}

	// The job must still be present so the delete can be retried.
	if got := d.jobSnapshot().ID; got != 42 {
		t.Errorf("job ID = %d, want it retained as 42 for retry", got)
	}
}

// TestSuccessfulDeleteAdvances is the positive counterpart: once the backend
// confirms the delete, the daemon is free to look for the next job.
func TestSuccessfulDeleteAdvances(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDaemon(juggler.StatusFinished)
	d.ie = testEndpoint(srv.URL)
	d.mutateJob(func(j *juggler.Job) { j.ID = 42 })
	d.setFeeder(nil)

	d.finishJob()

	select {
	case s := <-d.statusChan:
		if s != juggler.StatusWaitingJob {
			t.Errorf("requested status %q, want %q", s, juggler.StatusWaitingJob)
		}
	case <-time.After(daemonTestTimeout):
		t.Error("daemon did not advance to WaitingJob after a successful delete")
	}
}

// internStub serves the /job/ get that the daemon polls, reporting whatever
// status the test sets.
func internStub(t *testing.T, status juggler.JobStatus, id int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"Success": true,
			"Content": &juggler.Job{ID: id, Status: status},
		}); err != nil {
			t.Errorf("encoding stub response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPausedJobSeesCancel covers a cancel arriving while the print is paused.
//
// A pause never times out, so it is the one state a job can sit in forever.
// Intern hands a cancel over by setting the row to Cancelling and waiting for
// the daemon to reap it, so a paused daemon that never reads the row back
// leaves the job wedged and the printer unusable.
func TestPausedJobSeesCancel(t *testing.T) {
	tests := []struct {
		name       string
		internSays juggler.JobStatus
		wantCancel bool
	}{
		{
			name:       "cancel is picked up while paused",
			internSays: juggler.StatusCancelling,
			wantCancel: true,
		},
		{
			name:       "a paused job with no cancel stays paused",
			internSays: juggler.StatusPaused,
			wantCancel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const jobID = 42
			srv := internStub(t, tt.internSays, jobID)

			d := newTestDaemon(juggler.StatusPaused)
			d.ie = testEndpoint(srv.URL)
			d.mutateJob(func(j *juggler.Job) { j.ID = jobID })

			d.pollPausedJob()

			var got juggler.JobStatus
			select {
			case got = <-d.statusChan:
			default:
			}

			if tt.wantCancel {
				if got != juggler.StatusCancelling {
					t.Errorf("requested status = %q, want %q", got, juggler.StatusCancelling)
				}
				return
			}
			if got == juggler.StatusCancelling {
				t.Error("a paused job with no cancel on the row was cancelled")
			}
		})
	}
}

// TestPausedJobSurvivesInternFailure covers intern being unreachable while a
// print is paused. A failed fetch must never be read as a cancel: doing so
// would destroy a live print on a network blip.
func TestPausedJobSurvivesInternFailure(t *testing.T) {
	speedUpRetries(t)

	tests := []struct {
		name string
		uri  string
		srv  func(t *testing.T) *httptest.Server
	}{
		{
			name: "connection refused",
			uri:  "http://127.0.0.1:1",
		},
		{
			name: "intern returns 500",
			srv: func(t *testing.T) *httptest.Server {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(s.Close)
				return s
			},
		},
		{
			name: "intern returns garbage",
			srv: func(t *testing.T) *httptest.Server {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if _, err := io.WriteString(w, "not json"); err != nil {
						t.Errorf("writing stub response: %v", err)
					}
				}))
				t.Cleanup(s.Close)
				return s
			},
		},
		{
			name: "queue is empty",
			srv: func(t *testing.T) *httptest.Server {
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if err := json.NewEncoder(w).Encode(map[string]any{
						"Success": true,
						"Content": nil,
					}); err != nil {
						t.Errorf("encoding stub response: %v", err)
					}
				}))
				t.Cleanup(s.Close)
				return s
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri := tt.uri
			if tt.srv != nil {
				uri = tt.srv(t).URL
			}

			d := newTestDaemon(juggler.StatusPaused)
			d.ie = testEndpoint(uri)
			d.mutateJob(func(j *juggler.Job) { j.ID = 42 })

			if d.pollPausedJob() {
				t.Error("a failed intern fetch was treated as a cancel")
			}
			select {
			case got := <-d.statusChan:
				t.Errorf("unexpected status change %q requested", got)
			default:
			}
		})
	}
}
