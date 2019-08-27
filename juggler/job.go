package juggler

import (
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
)

type JobStatus string

const (
	StatusWaitingJob    = JobStatus("Waiting for job")
	StatusWaitingButton = JobStatus("Waiting for a button")
	StatusPrinting      = JobStatus("Printing")
	StatusSending       = JobStatus("Sending to printer")
	StatusCancelling    = JobStatus("Cancelling")
	StatusFinished      = JobStatus("Finished")
	StatusButtonTimeout = JobStatus("Button timeout")
	StatusPaused        = JobStatus("Paused")
)

type Job struct {
	Id           int                `json:"id"`
	Filename     string             `json:"file_name"`
	FileContent  string             `json:"file_content"`
	Owner        string             `json:"owner"`
	Status       JobStatus          `json:"status"`
	Progress     float64            `json:"progress"`
	Fetched      time.Time          `json:"fetched"`
	Scheduled    time.Time          `json:"scheduled"`
	FeederStatus gcodefeeder.Status `json:"-"`
}
