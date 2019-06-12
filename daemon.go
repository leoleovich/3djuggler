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
	feeder          *gcodefeeder.Feeder
}

// InfoHandler gives provides with json containing job status and some other important fields
func (daemon *Daemon) InfoHandler(w http.ResponseWriter, r *http.Request) {
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

func (daemon *Daemon) checkButtonPressed() bool {
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
