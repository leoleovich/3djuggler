package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/leoleovich/3djuggler/gcodefeeder"
	"github.com/leoleovich/3djuggler/juggler"
	"net/http"
	"net/url"
	"time"

	log "github.com/sirupsen/logrus"
)

const maxHTTPRetries = 3
const requestTimeout = 60 * time.Second
const retryInterval = 5 * time.Second

func requestWithRetry(request *http.Request) (resp *http.Response, err error) {
	client := &http.Client{Timeout: requestTimeout}
	for i := 0; i < maxHTTPRetries; i++ {
		resp, err = client.Do(request)
		if err == nil {
			return resp, nil
		}
		time.Sleep(retryInterval)
	}
	return nil, err
}

func (ie *InternEnpoint) reportJobStatusChange(job *juggler.Job) error {
	// Don't report default daemon status
	// TODO: think about separation of daemon and job statuses
	if job.Status == juggler.StatusWaitingJob {
		return nil
	}

	statusWithProgress := string(job.Status)
	// Detailed message if needed
	if job.Status == juggler.StatusPrinting && job.FeederStatus == gcodefeeder.Printing {
		sofar := job.Progress
		statusWithProgress = fmt.Sprintf("Printing... (%0.1f%%)", sofar)
	} else if job.Status == juggler.StatusPaused {
		switch job.FeederStatus {
		case gcodefeeder.MMUBusy:
			statusWithProgress = "Printing paused: MMU paused printing"
		case gcodefeeder.FSensorBusy:
			statusWithProgress = "Printing paused: Filament sensor paused printing"
		case gcodefeeder.ManuallyPaused:
			statusWithProgress = "Printing paused manually"
		}
	}

	log.Infof("Updating intern status to '%s'", statusWithProgress)

	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "update")
	data.Add("status", statusWithProgress)
	data.Add("id", fmt.Sprintf("%d", job.Id))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	return nil
}

func (ie *InternEnpoint) reschedule() error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "reschedule")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/printer/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (ie *InternEnpoint) deleteJob(job *juggler.Job) error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "delete")
	data.Add("id", fmt.Sprintf("%d", job.Id))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (ie *InternEnpoint) nextJob() error {
	return ie.getJob(0)
}

func (ie *InternEnpoint) getJob(id int) error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "get")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)
	if id != 0 {
		data.Add("id", fmt.Sprint(id))
	}

	req, err := http.NewRequest("POST", ie.Api_uri+"/job/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("bad response status from intern endpoint: %d", resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	var result struct {
		Success bool
		Content *juggler.Job
		Error   string
	}
	err = dec.Decode(&result)
	if err != nil {
		return err
	}
	if !result.Success {
		return fmt.Errorf("job %v action 'get' unsuccessful: %v", id, result.Error)
	}
	ie.job = result.Content

	if ie.job.Id == 0 {
		return errors.New("Nothing to print")
	}

	return nil
}

func (ie *InternEnpoint) reportStat() error {
	data := url.Values{}
	data.Set("app", ie.Api_app)
	data.Add("token", ie.Api_key)
	data.Add("action", "heartbeat")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest("POST", ie.Api_uri+"/printer/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
