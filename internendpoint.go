package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
	"github.com/leoleovich/3djuggler/juggler"

	log "github.com/sirupsen/logrus"
)

const maxHTTPRetries = 3
const requestTimeout = 60 * time.Second

// retryInterval is how long to wait between failed HTTP attempts.
var retryInterval = 5 * time.Second

// ErrNothingToPrint is returned by getJob when the queue holds no job for us.
// It is an expected condition, not a failure.
var ErrNothingToPrint = errors.New("nothing to print")

// httpClient is the client used for all intern endpoint calls. It is a
// package variable so tests can point it at a stub server.
var httpClient = &http.Client{Timeout: requestTimeout}

// post sends form data to the given intern endpoint path, retrying on
// transport errors. The body is rebuilt for every attempt: an http.Request
// body is a one-shot reader, so replaying the same request would have sent an
// empty body on every retry after the first.
func post(uri string, data url.Values) (*http.Response, error) {
	encoded := data.Encode()

	var lastErr error
	for i := 0; i < maxHTTPRetries; i++ {
		if i > 0 {
			time.Sleep(retryInterval)
		}

		req, err := http.NewRequest(http.MethodPost, uri, strings.NewReader(encoded))
		if err != nil {
			// A malformed URL will not fix itself on retry.
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			log.Debugf("attempt %d/%d failed: %v", i+1, maxHTTPRetries, err)
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("all %d attempts failed: %w", maxHTTPRetries, lastErr)
}

// postAndDiscard performs a fire-and-forget call and verifies the status code.
func postAndDiscard(uri string, data url.Values) error {
	resp, err := post(uri, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain a bounded amount so the error is diagnosable.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("bad response status from intern endpoint: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// values returns the form fields common to every intern endpoint call.
func (ie *InternEndpoint) values(action string) url.Values {
	data := url.Values{}
	data.Set("app", ie.APIApp)
	data.Add("token", ie.APIKey)
	data.Add("action", action)
	data.Add("printer_name", ie.PrinterName)
	data.Add("office_name", ie.OfficeName)
	return data
}

// statusForIntern renders the job status as the human-readable string the
// intern endpoint displays in the queue.
func statusForIntern(job *juggler.Job) string {
	if job.Status == juggler.StatusPrinting && job.FeederStatus == gcodefeeder.Printing {
		return fmt.Sprintf("Printing... (%0.1f%%)", job.Progress)
	}
	if job.Status == juggler.StatusPaused {
		switch job.FeederStatus {
		case gcodefeeder.MMUBusy:
			return "Printing paused: MMU paused printing"
		case gcodefeeder.FSensorBusy:
			return "Printing paused: Filament sensor paused printing"
		case gcodefeeder.ManuallyPaused:
			return "Printing paused manually"
		}
	}
	return string(job.Status)
}

func (ie *InternEndpoint) reportJobStatusChange(job *juggler.Job) error {
	// Don't report default daemon status
	// TODO: think about separation of daemon and job statuses
	if job.Status == juggler.StatusWaitingJob {
		return nil
	}

	statusWithProgress := statusForIntern(job)
	log.Infof("Updating intern status to '%s'", statusWithProgress)

	data := ie.values("update")
	data.Add("status", statusWithProgress)
	data.Add("id", fmt.Sprintf("%d", job.ID))

	return postAndDiscard(ie.APIURI+"/job/", data)
}

func (ie *InternEndpoint) reschedule() error {
	return postAndDiscard(ie.APIURI+"/printer/", ie.values("reschedule"))
}

func (ie *InternEndpoint) deleteJob(job *juggler.Job) error {
	data := ie.values("delete")
	data.Add("id", fmt.Sprintf("%d", job.ID))

	return postAndDiscard(ie.APIURI+"/job/", data)
}

func (ie *InternEndpoint) nextJob() error {
	return ie.getJob(0)
}

func (ie *InternEndpoint) getJob(id int) error {
	data := ie.values("get")
	if id != 0 {
		data.Add("id", fmt.Sprint(id))
	}

	resp, err := post(ie.APIURI+"/job/", data)
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
	if err := dec.Decode(&result); err != nil {
		return fmt.Errorf("decoding intern response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("job %v action 'get' unsuccessful: %v", id, result.Error)
	}
	// A successful response with no job means the queue is empty. Keep the
	// previously fetched job untouched rather than nil-ing it out.
	if result.Content == nil {
		return ErrNothingToPrint
	}
	ie.job = result.Content

	if ie.job.ID == 0 {
		return ErrNothingToPrint
	}

	return nil
}

func (ie *InternEndpoint) reportStat() error {
	return postAndDiscard(ie.APIURI+"/printer/", ie.values("heartbeat"))
}
