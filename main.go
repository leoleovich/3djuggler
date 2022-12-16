package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/leoleovich/3djuggler/juggler"
	log "github.com/sirupsen/logrus"
	"io"
	"os"
	"time"
)

var (
	jobfile                  = "/tmp/job"
	waitingForButtonInterval = 10 * time.Minute
	pollingInterval          = 5 * time.Second
	defaultListen            = "[::1]:8888"
	defaultSerial            = "/dev/ttyACM0"
	// Set during compilation to export version via /version http handler
	gitCommit = ""
)

type InternEndpoint struct {
	APIApp      string `json:"api_app"`
	APIKey      string `json:"api_key"`
	APIURI      string `json:"api_uri"`
	PrinterName string `json:"printerName"`
	OfficeName  string `json:"officeName"`
	job         *juggler.Job
}

type Config struct {
	Listen string
	Serial string
	// preserve the typo for backward compatibility
	InternEndpoint *InternEndpoint `json:"InternEnpoint"`
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

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
	if err == nil {
		log.SetOutput(file)
	} else {
		log.Fatal("Failed to log to file")
	}
	defer file.Close()

	daemon := &Daemon{
		config: &Config{},
		job:    &juggler.Job{Status: juggler.StatusWaitingJob},
	}

	jsonFile, err := os.Open(configFile)
	if err != nil {
		log.Fatalf("Can't open main config: %v", err)
	}
	byteValue, err := io.ReadAll(jsonFile)
	if err != nil {
		panic(fmt.Sprintf("Can't open main config: %v", err))
	}
	err = json.Unmarshal(byteValue, &daemon.config)
	if err != nil {
		panic(fmt.Sprintf("Can't decode main config: %v", err))
	}
	jsonFile.Close()
	fmt.Printf("config: %+v\n", daemon.config.InternEndpoint)

	if daemon.config.Listen == "" {
		daemon.config.Listen = defaultListen
	}
	if daemon.config.Serial == "" {
		daemon.config.Serial = defaultSerial
	}

	daemon.jobfile = jobfile

	daemon.ie = &InternEndpoint{
		APIApp: daemon.config.InternEndpoint.APIApp,
		APIKey: daemon.config.InternEndpoint.APIKey,
		APIURI: daemon.config.InternEndpoint.APIURI,

		PrinterName: daemon.config.InternEndpoint.PrinterName,
		OfficeName:  daemon.config.InternEndpoint.OfficeName,
		job:         &juggler.Job{},
	}

	daemon.Start()
}
