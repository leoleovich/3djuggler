package main

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
	"github.com/leoleovich/3djuggler/juggler"
	log "github.com/sirupsen/logrus"
)

// statusChangeTimeout bounds how long a handler waits for the polling loop to
// pick up a requested status change before giving up.
var statusChangeTimeout = 30 * time.Second

// statusPollInterval is how often a handler re-checks for the status change.
var statusPollInterval = 100 * time.Millisecond

type Daemon struct {
	config     *Config
	ie         *InternEndpoint
	statusChan chan juggler.JobStatus

	// mu guards job and feeder, which are read by HTTP handlers while the
	// polling loop writes them.
	mu     sync.RWMutex
	job    *juggler.Job
	feeder *gcodefeeder.Feeder
}

// jobSnapshot returns a copy of the current job, safe to read outside the lock.
func (daemon *Daemon) jobSnapshot() juggler.Job {
	daemon.mu.RLock()
	defer daemon.mu.RUnlock()
	return *daemon.job
}

func (daemon *Daemon) jobStatus() juggler.JobStatus {
	daemon.mu.RLock()
	defer daemon.mu.RUnlock()
	return daemon.job.Status
}

func (daemon *Daemon) currentFeeder() *gcodefeeder.Feeder {
	daemon.mu.RLock()
	defer daemon.mu.RUnlock()
	return daemon.feeder
}

func (daemon *Daemon) setFeeder(f *gcodefeeder.Feeder) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.feeder = f
}

// mutateJob applies fn to the job under the write lock.
func (daemon *Daemon) mutateJob(fn func(*juggler.Job)) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	fn(daemon.job)
}

func (daemon *Daemon) registerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/info", daemon.InfoHandler)
	mux.HandleFunc("/start", daemon.StartHandler)
	mux.HandleFunc("/pause", daemon.PauseHandler)
	mux.HandleFunc("/reschedule", daemon.RescheduleHandler)
	mux.HandleFunc("/cancel", daemon.CancelHandler)
	mux.HandleFunc("/version", daemon.VersionHandler)
}

func (daemon *Daemon) Start() {
	var err error
	if daemon.config.Insecure {
		log.Warning("TLS certificate verification is DISABLED (-insecure)")
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in via -insecure flag
	}

	mux := http.NewServeMux()
	daemon.registerHandlers(mux)
	server := &http.Server{
		Addr:              daemon.config.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { log.Fatal(server.ListenAndServe()) }()
	log.Debug("Started http server on ", daemon.config.Listen)

	daemon.statusChan = make(chan juggler.JobStatus, 10)
	var oldStatus juggler.JobStatus

	if err := daemon.ie.reschedule(); err != nil {
		log.Error("reschedule failed: ", err)
	}
	for range time.Tick(pollingInterval) {
		select {
		case status := <-daemon.statusChan:
			daemon.mutateJob(func(j *juggler.Job) { j.Status = status })
			log.Debugf("Assigning status '%s'", status)
			job := daemon.jobSnapshot()
			if err := daemon.ie.reportJobStatusChange(&job); err != nil {
				log.Error("Can't report it to intern: ", err)
			}
		default:
			log.Debug("No status updates")
		}
		log.Infof("My status is: '%s'", daemon.jobStatus())

		if err = daemon.ie.reportStat(); err != nil {
			log.Error(err)
		}

		switch daemon.jobStatus() {
		case juggler.StatusWaitingJob, juggler.StatusButtonTimeout:
			daemon.mutateJob(func(j *juggler.Job) { j.ID = 0 })
			if err = daemon.ie.nextJob(); err != nil {
				// An empty queue is the normal case, not an error.
				if errors.Is(err, ErrNothingToPrint) {
					log.Debug("Nothing to print")
				} else {
					log.Error(err)
				}
				break
			}
			daemon.mutateJob(func(j *juggler.Job) {
				j.ID = daemon.ie.job.ID
				j.Filename = daemon.ie.job.Filename
				j.FileContent = daemon.ie.job.FileContent
				j.Progress = daemon.ie.job.Progress
				j.Owner = daemon.ie.job.Owner
				j.Color = daemon.ie.job.Color
				j.Fetched = time.Now()
				j.Scheduled = time.Now().Add(waitingForButtonInterval)
			})

			daemon.UpdateStatus(juggler.StatusWaitingButton)
			fallthrough

		case juggler.StatusWaitingButton:
			job := daemon.jobSnapshot()
			log.Info("Job ", job.ID, " is waiting")
			err = daemon.ie.getJob(job.ID)
			if err != nil {
				log.Error("Can't get job status from intern: ", err)
			} else {
				log.Info("Job status on intern: ", daemon.ie.job.Status)
			}
			if err == nil && daemon.ie.job.Status == juggler.StatusCancelling {
				log.Info("The job is cancelling")
				daemon.UpdateStatus(juggler.StatusCancelling)
				break
			}

			if job.Scheduled.After(time.Now()) {
				log.Info("Waiting ", int(time.Until(job.Scheduled).Seconds()), " more seconds for somebody to press the button")
			} else {
				log.Warning("Nobody pressed the button on time")
				log.Warning("Timeout while waiting for a job. Switching back to ", job.Status)
				daemon.UpdateStatus(juggler.StatusButtonTimeout)
			}

		case juggler.StatusSending:
			if oldStatus != juggler.StatusWaitingButton && oldStatus != juggler.StatusPaused {
				log.Warningf("Forbidden status change sequence, from %s to %s. Ignoring", oldStatus, daemon.jobStatus())
				continue
			}

			job := daemon.jobSnapshot()
			log.Info("Sending to printer")
			log.Debug("FileSize: ", len(job.FileContent))

			feeder, ferr := gcodefeeder.NewFeeder(
				daemon.config.Serial,
				strings.NewReader(job.FileContent),
			)
			if ferr != nil {
				log.Error("Failed to create Feeder: ", ferr)
				// Without a feeder the job can never print. Tell intern
				// instead of silently retrying forever.
				daemon.UpdateStatus(juggler.StatusCancelling)
				break
			}
			daemon.setFeeder(feeder)
			daemon.UpdateStatus(juggler.StatusPrinting)

			go func() {
				if err := feeder.Feed(); err != nil {
					log.Error(err)
				}
			}()

		case juggler.StatusPrinting:
			job := daemon.jobSnapshot()
			log.Infof("Job %d is currently printing", job.ID)
			// Check status from intern
			err = daemon.ie.getJob(job.ID)
			if err != nil {
				log.Error("Can't report status to intern: ", err)
			}
			if err == nil && daemon.ie.job.Status == juggler.StatusCancelling {
				log.Info("Cancelling the job")
				daemon.UpdateStatus(juggler.StatusCancelling)
				break
			}
			feeder := daemon.currentFeeder()
			if feeder == nil {
				log.Error("Printing without a feeder, cancelling")
				daemon.UpdateStatus(juggler.StatusCancelling)
				break
			}
			feederStatus := feeder.Status()
			progress := float64(feeder.Progress())
			daemon.mutateJob(func(j *juggler.Job) {
				j.Progress = progress
				j.FeederStatus = feederStatus
			})

			switch feederStatus {
			case gcodefeeder.Printing:
				// We need to update percentage of print
				snapshot := daemon.jobSnapshot()
				if err := daemon.ie.reportJobStatusChange(&snapshot); err != nil {
					log.Error("Can't report it to intern: ", err)
				}
			case gcodefeeder.Finished:
				daemon.UpdateStatus(juggler.StatusFinished)
			case gcodefeeder.Error:
				daemon.UpdateStatus(juggler.StatusCancelling)
			case gcodefeeder.ManuallyPaused, gcodefeeder.FSensorBusy, gcodefeeder.MMUBusy:
				daemon.UpdateStatus(juggler.StatusPaused)
			default:
				log.Warning("Printing. Feeder status is: ", feederStatus)
			}
		case juggler.StatusPaused:
			feeder := daemon.currentFeeder()
			if feeder == nil {
				log.Error("Paused without a feeder, cancelling")
				daemon.UpdateStatus(juggler.StatusCancelling)
				break
			}
			feederStatus := feeder.Status()
			daemon.mutateJob(func(j *juggler.Job) { j.FeederStatus = feederStatus })
			log.Infof("Job %d is currently paused", daemon.jobSnapshot().ID)
			switch feederStatus {
			case gcodefeeder.Printing:
				daemon.UpdateStatus(juggler.StatusPrinting)
			case gcodefeeder.Error:
				daemon.UpdateStatus(juggler.StatusCancelling)
			default:
				log.Warning("Paused. Feeder status is: ", feederStatus)
			}
		case juggler.StatusCancelling, juggler.StatusFinished:
			daemon.finishJob()
		default:
			log.Error("Job ", daemon.jobSnapshot(), " is in a weird state")
		}

		oldStatus = daemon.jobStatus()
	}
}

func (daemon *Daemon) UpdateStatus(status juggler.JobStatus) {
	select {
	case daemon.statusChan <- status:
		log.Debugf("Requesting status change to: '%s'", status)
	default:
		log.Error("Unable to request status change. statusChan is full")
	}
}

// finishJob runs the terminal state: stop the feeder, delete the job from
// intern, and only then allow the daemon to look for more work. It is a
// method so the "do not advance on a failed delete" rule can be tested
// without driving the whole polling loop.
func (daemon *Daemon) finishJob() {
	if feeder := daemon.currentFeeder(); feeder != nil && feeder.Status() != gcodefeeder.Finished {
		log.Info("Stopping feeder")
		feeder.Cancel()
	}

	log.Info("Deleting from intern")
	job := daemon.jobSnapshot()
	if err := daemon.ie.deleteJob(&job); err != nil {
		// Do not advance. The backend may still hold this job, and fetching
		// again from WaitingJob would print it a second time. Stay in the
		// terminal state and retry on the next tick.
		log.Errorf("Failed to delete job %d, will retry: %v", job.ID, err)
		return
	}
	daemon.setFeeder(nil)
	daemon.UpdateStatus(juggler.StatusWaitingJob)
}

// awaitStatus blocks until the polling loop has applied the expected status,
// or the timeout expires. Returns false on timeout.
func (daemon *Daemon) awaitStatus(expected juggler.JobStatus) bool {
	deadline := time.Now().Add(statusChangeTimeout)
	for time.Now().Before(deadline) {
		if daemon.jobStatus() == expected {
			return true
		}
		log.Infof("Waiting for %s status to be set", expected)
		time.Sleep(statusPollInterval)
	}
	log.Errorf("Timed out waiting for status %s", expected)
	return false
}

// InfoHandler gives provides with json containing job status and some other important fields
func (daemon *Daemon) InfoHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received info handler request")
	// Add headers to allow AJAX
	juggler.SetJSONHeaders(w)

	current := daemon.jobSnapshot()
	job := &juggler.Job{
		ID:          current.ID,
		Owner:       current.Owner,
		Filename:    current.Filename,
		Progress:    current.Progress,
		Status:      current.Status,
		Color:       current.Color,
		Fetched:     current.Fetched,
		Scheduled:   current.Scheduled,
		PrinterName: daemon.config.InternEndpoint.PrinterName,
	}

	b, err := json.Marshal(job)
	if err != nil {
		log.Errorf("Failed to respond on /info request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(b))
}

// StartHandler acknowledged start of the job
func (daemon *Daemon) StartHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received start handler request")
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	switch daemon.jobStatus() {
	case juggler.StatusWaitingButton:
		// Initial start
		daemon.UpdateStatus(juggler.StatusSending)
		if !daemon.awaitStatus(juggler.StatusSending) {
			http.Error(w, "timed out waiting for job to start", http.StatusServiceUnavailable)
		}
		return
	case juggler.StatusPaused:
		// Unpause
		feeder := daemon.currentFeeder()
		if feeder == nil {
			http.Error(w, "no active print to resume", http.StatusConflict)
			return
		}
		feeder.Start()
		daemon.UpdateStatus(juggler.StatusPrinting)
		if !daemon.awaitStatus(juggler.StatusPrinting) {
			http.Error(w, "timed out waiting for job to resume", http.StatusServiceUnavailable)
		}
		return
	}

	errS := fmt.Sprintf("Ignore buttonpress in '%v' status", daemon.jobStatus())
	log.Info(errS)
	http.Error(w, errS, http.StatusBadRequest)
}

// RescheduleHandler resets the time when the job will start
func (daemon *Daemon) RescheduleHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received reschedule handler request")
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.jobStatus() != juggler.StatusWaitingButton {
		errS := fmt.Sprintf("Ignore reschedule in '%v' status", daemon.jobStatus())
		log.Info(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.mutateJob(func(j *juggler.Job) {
		j.Fetched = time.Now()
		j.Scheduled = time.Now().Add(waitingForButtonInterval)
	})
}

// CancelHandler cancels job execution
func (daemon *Daemon) CancelHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received cancel handler request")
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.jobSnapshot().ID == 0 {
		errS := "Ignore cancel, no job scheduled"
		log.Info(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.mutateJob(func(j *juggler.Job) { j.Scheduled = time.Time{} })
	daemon.UpdateStatus(juggler.StatusCancelling)
	// The loop moves from Cancelling to WaitingJob on its own, so either
	// state means the cancel was accepted.
	if !daemon.awaitStatusAny(juggler.StatusCancelling, juggler.StatusWaitingJob) {
		http.Error(w, "timed out waiting for job to cancel", http.StatusServiceUnavailable)
	}
}

// awaitStatusAny blocks until the job reaches any of the expected statuses.
func (daemon *Daemon) awaitStatusAny(expected ...juggler.JobStatus) bool {
	deadline := time.Now().Add(statusChangeTimeout)
	for time.Now().Before(deadline) {
		current := daemon.jobStatus()
		for _, e := range expected {
			if current == e {
				return true
			}
		}
		log.Infof("Waiting for one of %v status to be set", expected)
		time.Sleep(statusPollInterval)
	}
	log.Errorf("Timed out waiting for any of statuses %v", expected)
	return false
}

// PauseHandler pauses job execution
func (daemon *Daemon) PauseHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received pause handler request")
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.jobStatus() != juggler.StatusPrinting {
		errS := "Ignore pause, not printing"
		log.Info(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	feeder := daemon.currentFeeder()
	if feeder == nil {
		http.Error(w, "no active print to pause", http.StatusConflict)
		return
	}
	feeder.Pause()
	daemon.UpdateStatus(juggler.StatusPaused)
	if !daemon.awaitStatus(juggler.StatusPaused) {
		http.Error(w, "timed out waiting for job to pause", http.StatusServiceUnavailable)
	}
}

// VersionHandler reports the git commit this daemon was built from
func (daemon *Daemon) VersionHandler(w http.ResponseWriter, _ *http.Request) {
	log.Infof("Received version handler request")
	// Add headers to allow AJAX
	juggler.SetHeaders(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, gitCommit)
}
