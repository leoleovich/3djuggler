package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/leoleovich/3djuggler/juggler"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"time"
)

var (
	jobfile                  = "/tmp/job"
	waitingForButtonInterval = time.Duration(10 * time.Minute)
	pollingInterval          = time.Duration(15 * time.Second)
	defaultListen            = "[::1]:8888"
	defaultSerial            = "/dev/ttyACM0"
)

type InternEnpoint struct {
	Api_app     string
	Api_key     string
	Api_uri     string
	job         *juggler.Job
	PrinterName string
	OfficeName  string
	log         *log.Logger
}

type Config struct {
	Listen        string
	Serial        string
	InternEnpoint *InternEnpoint
}

func main() {
	var err error
	var configFile, logFile string
	var verbose bool

	flag.StringVar(&configFile, "config", "3djuggler.json", "Main config")
	flag.StringVar(&logFile, "log", "/var/log/3djuggler.log", "Where to log")
	flag.BoolVar(&verbose, "verbose", false, "Use verbose log output")
	flag.Parse()

	if verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.SetOutput(os.Stdout)

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Fatal("Failed to log to file")
	}
	defer file.Close()

	daemon := &Daemon{
		timer:  time.NewTimer(0),
		config: &Config{},
		job:    &juggler.Job{Status: juggler.StatusWaitingJob},
	}

	jsonFile, err := os.Open(configFile)
	if err != nil {
		log.Fatalf("Can't open main config: %v", err)
	}
	byteValue, err := ioutil.ReadAll(jsonFile)
	if err != nil {
		panic(fmt.Sprintf("Can't open main config: %v", err))
	}
	err = json.Unmarshal(byteValue, &daemon.config)
	if err != nil {
		panic(fmt.Sprintf("Can't decode main config: %v", err))
	}
	jsonFile.Close()

	if daemon.config.Listen == "" {
		daemon.config.Listen = defaultListen
	}
	if daemon.config.Serial == "" {
		daemon.config.Serial = defaultSerial
	}

	daemon.jobfile = jobfile

	daemon.ie = &InternEnpoint{
		Api_app: daemon.config.InternEnpoint.Api_app,
		Api_key: daemon.config.InternEnpoint.Api_key,
		Api_uri: daemon.config.InternEnpoint.Api_uri,

		PrinterName: daemon.config.InternEnpoint.PrinterName,
		OfficeName:  daemon.config.InternEnpoint.OfficeName,
		job:         &juggler.Job{},
	}

	daemon.Start()
}
