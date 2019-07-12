package main

import (
	"encoding/json"
	"fmt"
	"github.com/leoleovich/go-gcodefeeder/gcodefeeder"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"time"
)

type Daemon struct {
	timer      *time.Timer
	config     *Config
	buttonfile string
	// gizmostatusfile is used to communicate with the device daemon is running on
	gizmostatusfile string
	jobfile         string
	job             *Job
	ie              *InternEnpoint
	feeder          *gcodefeeder.Feeder
}

func (daemon *Daemon) UpdateStatus(status string) {
	log.Infof("Updating intern status to %s", status)
	daemon.job.Status = status
	if err := daemon.ie.reportJobStatusChange(daemon.job); err != nil {
		log.Error("Can't report it to intern: ", err)
	}
}

// InfoHandler gives provides with json containing job status and some other important fields
func (daemon *Daemon) InfoHandler(w http.ResponseWriter, r *http.Request) {
	// Add headers to allow AJAX
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Content-Type", "text/json")

	job := &Job{
		Owner:    daemon.job.Owner,
		Filename: daemon.job.Filename,
		Progress: daemon.job.Progress,
		Status:   daemon.job.Status,
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
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET")
	w.Header().Set("Content-Type", "text/json")

	if daemon.job.Status != "Waiting for a button" {
		errS := fmt.Sprintf("Ignore buttonpress in %v status", daemon.job.Status)
		log.Infof(errS)
		http.Error(w, errS, http.StatusBadRequest)
		return
	}

	daemon.UpdateStatus("Sending to printer")
	w.WriteHeader(200)
}

func (daemon *Daemon) checkButtonPressed() bool {
	if daemon.job.Status == "Sending to printer" {
		return true
	}

	// Keep file handler for the migration period
	defer os.Remove(daemon.buttonfile)
	s, err := os.Stat(daemon.buttonfile)
	if err != nil {
		return false
	}

	if s.ModTime().Add(waitingForButtonInterval).After(time.Now()) {
		return true
	}
	return false
}
