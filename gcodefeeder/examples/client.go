package main

import (
	"os"
	"strings"
	"time"

	"github.com/leoleovich/3djuggler/gcodefeeder"
	log "github.com/sirupsen/logrus"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	feeder, _ := gcodefeeder.NewFeeder(
		"/dev/tty.usbmodem14601",
		strings.NewReader("M140 S0\nM104 S0\nM107\nM84 X Y E\n"),
	)
	go func() {
		for {
			log.Debug("Progress: ", feeder.Progress(), " Status: ", feeder.Status())
			time.Sleep(1 * time.Second)
		}
	}()

	if err := feeder.Feed(); err != nil {
		log.Fatal(err)
	}
}
