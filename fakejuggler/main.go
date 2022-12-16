package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/leoleovich/3djuggler/juggler"
	"net/http"
)

func usage() {
	fmt.Println(`
pr: progress job by 10%
s: start job
p: pause job
f: finish the job
w: waiting for job
b: waiting for a button`)
}

type FakeJuggler struct {
	Job *juggler.Job
}

func (j *FakeJuggler) start() {
	switch j.Job.Status {
	case juggler.StatusSending:
		log.Println("Asked to start when already printing")
	case juggler.StatusPrinting:
		log.Println("Asked to start when already printing")
	case juggler.StatusCancelling:
		log.Println("Asked to start when cancelling")
	case juggler.StatusFinished:
		log.Println("Asked to start when finished")
	}
	j.Job.Progress = 0
	j.Job.Status = juggler.StatusPrinting
}
func (j *FakeJuggler) cancel() {
	switch j.Job.Status {
	case juggler.StatusWaitingJob:
		log.Println("Asked to cancel when waiting for job")
	case juggler.StatusWaitingButton:
		log.Println("Asked to cancel when waiting for button")
	case juggler.StatusFinished:
		log.Println("Asked to cancel when finished")
	}
	j.Job.Status = juggler.StatusCancelling
}

func (j *FakeJuggler) finish() {
	j.Job.Status = juggler.StatusFinished
}

func (j *FakeJuggler) reschedule() {
	j.Job.Fetched = time.Now()
	j.Job.Scheduled = time.Now().Add(600 * time.Second)
}

func (j *FakeJuggler) waitForButton() {
	j.Job.Progress = 0
	j.Job.ID = 10
	j.Job.Status = juggler.StatusWaitingButton
	j.Job.Fetched = time.Now()
	j.Job.Scheduled = time.Now().Add(600 * time.Second)
	j.Job.Color = "Red"
}

func (j *FakeJuggler) waitForJob() {
	j.Job.Progress = 0
	j.Job.ID = 0
	j.Job.Status = juggler.StatusWaitingJob
}

func (j *FakeJuggler) progress() {
	j.Job.Status = juggler.StatusPrinting
	next := j.Job.Progress + 10.0
	if next > 100 {
		next = 100.0
		j.Job.Status = juggler.StatusFinished
	}
	j.Job.Progress = next
	log.Printf("Updated progress to %.1f%%", next)
}

func (j *FakeJuggler) pause() {
	j.Job.Status = juggler.StatusPaused
}

func main() {
	job := juggler.Job{
		Status:   juggler.StatusWaitingJob,
		Owner:    "user",
		Filename: "some_file.gcode",
	}
	j := FakeJuggler{Job: &job}

	http.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		log.Println("cancel")
		j.cancel()
	})

	http.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		log.Println("start")
		j.start()
	})

	http.HandleFunc("/reschedule", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		log.Println("reschedule")
		j.reschedule()
	})

	http.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		b, err := json.Marshal(j.Job)
		if err != nil {
			panic(err)
		}
		_, err = w.Write(b)
		if err != nil {
			panic(err)
		}
	})

	http.HandleFunc("/pause", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		log.Println("pause")
		j.pause()
	})

	http.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		juggler.SetHeaders(w)
		log.Println("version")
		fmt.Fprintf(w, "12345")
	})

	go func() {
		if err := http.ListenAndServe(":8888", nil); err != nil {
			log.Fatalf("serving HTTP: %v", err)
		}
	}()

	reader := bufio.NewReader(os.Stdin)
	usage()
	fmt.Printf("Current status: '%s'\n", j.Job.Status)

	for {
		input, _ := reader.ReadString('\n')
		c := strings.TrimSpace(input)
		switch c {
		case "pr":
			j.progress()
		case "w":
			j.waitForJob()
		case "b":
			j.waitForButton()
		case "s":
			j.start()
		case "p":
			j.pause()
		case "f":
			j.finish()
		default:
			usage()
		}
		fmt.Printf("New status: '%s'\n", j.Job.Status)
	}
}
