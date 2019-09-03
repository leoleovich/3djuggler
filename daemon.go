package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
	"github.com/leoleovich/3djuggler/juggler"
	log "github.com/sirupsen/logrus"
)

type Daemon struct {
	timer      *time.Timer
	config     *Config
	jobfile    string
	job        *juggler.Job
	ie         *InternEnpoint
	feeder     *gcodefeeder.Feeder
	statusChan chan juggler.JobStatus
}

func (daemon *Daemon) Start() {
	var err error
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	http.HandleFunc("/info", daemon.InfoHandler)
	http.HandleFunc("/start", daemon.StartHandler)
	http.HandleFunc("/pause", daemon.PauseHandler)
	http.HandleFunc("/reschedule", daemon.RescheduleHandler)
	http.HandleFunc("/cancel", daemon.CancelHandler)
	go func() { log.Fatal(http.ListenAndServe(daemon.config.Listen, nil)) }()
	log.Debug("Started http server on ", daemon.config.Listen)

	daemon.statusChan = make(chan juggler.JobStatus, 10)

	if err := daemon.ie.reschedule(); err != nil {
		log.Error("reschedule failed: ", err)
	}
	for {
		select {
		case <-daemon.timer.C:
			daemon.timer.Reset(pollingInterval)
			select {
			case daemon.job.Status = <-daemon.statusChan:
				log.Debugf("Assigning status '%s'", daemon.job.Status)
				if err := daemon.ie.reportJobStatusChange(daemon.job); err != nil {
					log.Error("Can't report it to intern: ", err)
				}
			default:
				log.Debug("No status updates")
			}
			log.Infof("My status is: '%s'", daemon.job.Status)

			if err = daemon.ie.reportStat(); err != nil {
				log.Error(err)
			}

			switch daemon.job.Status {
			case juggler.StatusWaitingJob, juggler.StatusButtonTimeout:
				daemon.job.Id = 0
				if err = daemon.ie.nextJob(); err != nil {
					log.Error(err)
					daemon.UpdateStatus(juggler.StatusWaitingJob)
					break
				}
				daemon.job.Id = daemon.ie.job.Id
				daemon.job.Filename = daemon.ie.job.Filename
				daemon.job.FileContent = daemon.ie.job.FileContent
				daemon.job.Progress = daemon.ie.job.Progress
				daemon.job.Owner = daemon.ie.job.Owner
				daemon.job.Fetched = time.Now()
				daemon.job.Scheduled = time.Now().Add(waitingForButtonInterval)

				daemon.UpdateStatus(juggler.StatusWaitingButton)
				fallthrough

			case juggler.StatusWaitingButton:
				log.Info("Job ", daemon.job.Id, " is waiting")
				err = daemon.ie.getJob(daemon.job.Id)
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

				if daemon.job.Scheduled.After(time.Now()) {
					log.Info("Waiting ", daemon.job.Scheduled.Unix()-time.Now().Unix(), " more seconds for somebody to press the button")
				} else {
					log.Warning("Nobody pressed the button on time")
					log.Warning("Timeout while waiting for a job. Switching back to ", daemon.job.Status)
					daemon.UpdateStatus(juggler.StatusButtonTimeout)
				}

			case juggler.StatusSending:
				log.Info("Sending to printer")

				log.Debug("FileSize: ", len(daemon.job.FileContent))
				err := ioutil.WriteFile(daemon.jobfile, []byte(daemon.job.FileContent), 0644)
				if err != nil {
					log.Error(err)
					break
				}

				daemon.feeder, err = gcodefeeder.NewFeeder(
					daemon.config.Serial,
					daemon.jobfile,
				)
				if err != nil {
					log.Error("Failed to create Feeder: ", err)
					break
				}
				daemon.UpdateStatus(juggler.StatusPrinting)

				go daemon.feeder.Feed()

			case juggler.StatusPrinting, juggler.StatusPaused:
				log.Info("Job ", daemon.job.Id, " is currently in progress")
				// Check status from intern
				err = daemon.ie.getJob(daemon.job.Id)
				if err != nil {
					log.Error("Can't report status to intern: ", err)
				}
				if err == nil && daemon.ie.job.Status == juggler.StatusCancelling {
					log.Info("Cancelling the job")
					daemon.UpdateStatus(juggler.StatusCancelling)
					break
				}
				daemon.job.Progress = float64(daemon.feeder.Progress())
				daemon.job.FeederStatus = daemon.feeder.Status()

				switch daemon.job.FeederStatus {
				case gcodefeeder.Printing:
					daemon.UpdateStatus(juggler.StatusPrinting)
				case gcodefeeder.Finished:
					daemon.UpdateStatus(juggler.StatusFinished)
				case gcodefeeder.Error:
					daemon.UpdateStatus(juggler.StatusCancelling)
				case gcodefeeder.ManuallyPaused, gcodefeeder.FSensorBusy, gcodefeeder.MMUBusy:
					daemon.UpdateStatus(juggler.StatusPaused)
				default:
					daemon.UpdateStatus(daemon.job.Status)
				}
			case juggler.StatusCancelling:
				fallthrough
			case juggler.StatusFinished:
				log.Info("Stopping feeder")
				if daemon.feeder != nil {
					daemon.feeder.Cancel()
				}
				daemon.feeder = nil

				log.Info("Deleting from intern")
				err = daemon.ie.deleteJob(daemon.job)
				if err != nil {
					log.Error(err)
				}
				daemon.UpdateStatus(juggler.StatusWaitingJob)
			default:
				log.Error("Job ", daemon.job, " is in a weird state")
			}

		}
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

// InfoHandler gives provides with json containing job status and some other important fields
func (daemon *Daemon) InfoHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	job := &juggler.Job{
		Id:        daemon.job.Id,
		Owner:     daemon.job.Owner,
		Filename:  daemon.job.Filename,
		Progress:  daemon.job.Progress,
		Status:    daemon.job.Status,
		Fetched:   daemon.job.Fetched,
		Scheduled: daemon.job.Scheduled,
	}

	b, err := json.Marshal(job)
	if err != nil {
		log.Errorf("Failed to respond on /info request: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, string(b))
}

// StartHandler acknowledged start of the job
func (daemon *Daemon) StartHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.job.Status == juggler.StatusWaitingButton {
		// Initial start
		daemon.UpdateStatus(juggler.StatusSending)
		return
	} else if daemon.job.Status == juggler.StatusPaused {
		// Unpause
		daemon.feeder.Start()
		daemon.UpdateStatus(juggler.StatusPrinting)
		return
	}

	errS := fmt.Sprintf("Ignore buttonpress in '%v' status", daemon.job.Status)
	log.Infof(errS)
	http.Error(w, errS, http.StatusBadRequest)
}

// RescheduleHandler resets the time when the job will start
func (daemon *Daemon) RescheduleHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.job.Status != juggler.StatusWaitingButton {
		errS := fmt.Sprintf("Ignore reschedule in '%v' status", daemon.job.Status)
		log.Infof(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.job.Fetched = time.Now()
	daemon.job.Scheduled = time.Now().Add(waitingForButtonInterval)
}

// CancelHandler cancels job execution
func (daemon *Daemon) CancelHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.job.Id == 0 {
		errS := fmt.Sprintf("Ignore cancel, no job scheduled")
		log.Infof(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.job.Scheduled = time.Time{}
	daemon.UpdateStatus(juggler.StatusCancelling)
}

// CancelHandler cancels job execution
func (daemon *Daemon) PauseHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	juggler.SetHeaders(w)

	if daemon.job.Status != juggler.StatusPrinting {
		errS := fmt.Sprintf("Ignore pause, not printing")
		log.Infof(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.feeder.Pause()
	daemon.UpdateStatus(juggler.StatusPaused)
}
