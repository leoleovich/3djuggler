// Package juggler holds the types shared between the daemon and any client
// that talks to its local HTTP API.
package juggler

import (
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
)

// JobStatus is the state of a print job. These strings are part of the wire
// contract with the backend and are displayed to users, so they must not be
// changed casually.
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

// Job is a single print: the file to print, who asked for it, and how far
// along it is.
type Job struct {
	ID       int    `json:"id"`
	Filename string `json:"file_name"`
	// FileContent is populated when fetching a job from the intern endpoint.
	// omitempty keeps it off the wire in the /info response, which must never
	// serve the gcode body.
	FileContent  string             `json:"file_content,omitempty"`
	Owner        string             `json:"owner"`
	Status       JobStatus          `json:"status"`
	Color        string             `json:"color"`
	Progress     float64            `json:"progress"`
	Fetched      time.Time          `json:"fetched"`
	Scheduled    time.Time          `json:"scheduled"`
	FeederStatus gcodefeeder.Status `json:"-"`
	PrinterName  string             `json:"printer_name"`
}
