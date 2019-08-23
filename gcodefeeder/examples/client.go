package main

import (
	"github.com/leoleovich/3djuggler/gcodefeeder"
	log "github.com/sirupsen/logrus"
	"os"
	"time"
)

func main() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)

	feeder, _ := gcodefeeder.NewFeeder(
		"/dev/tty.usbmodem14601",
		"/Users/leoleovich/3d/M5.0_Nut_0.3mm_PLA_MK3.gcode",
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
