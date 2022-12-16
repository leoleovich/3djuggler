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

func (ie *InternEndpoint) reportJobStatusChange(job *juggler.Job) error {
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
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", "update")
	data.Add("status", statusWithProgress)
	data.Add("id", fmt.Sprintf("%d", job.ID))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest(http.MethodPost, ie.APIURI+"/job/", bytes.NewBufferString(data.Encode()))
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

func (ie *InternEndpoint) reschedule() error {
	data := url.Values{}
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", "reschedule")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest(http.MethodPost, ie.APIURI+"/printer/", bytes.NewBufferString(data.Encode()))
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

func (ie *InternEndpoint) deleteJob(job *juggler.Job) error {
	data := url.Values{}
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", "delete")
	data.Add("id", fmt.Sprintf("%d", job.ID))
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest(http.MethodPost, ie.APIURI+"/job/", bytes.NewBufferString(data.Encode()))
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

func (ie *InternEndpoint) nextJob() error {
	return ie.getJob(0)
}

func (ie *InternEndpoint) getJob(id int) error {
	data := url.Values{}
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", "get")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)
	if id != 0 {
		data.Add("id", fmt.Sprint(id))
	}

	req, err := http.NewRequest(http.MethodPost, ie.APIURI+"/job/", bytes.NewBufferString(data.Encode()))
	if err != nil {
		return err
	}
	resp, err := requestWithRetry(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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

	if ie.job.ID == 0 {
		return errors.New("Nothing to print")
	}

	return nil
}

func (ie *InternEndpoint) reportStat() error {
	data := url.Values{}
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", "heartbeat")
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)

	req, err := http.NewRequest(http.MethodPost, ie.APIURI+"/printer/", bytes.NewBufferString(data.Encode()))
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
