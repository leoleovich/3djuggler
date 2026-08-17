package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/leoleovich/3djuggler/juggler"
	log "github.com/sirupsen/logrus"
)

var (
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

// String redacts the API key so the config can be logged safely.
func (ie *InternEndpoint) String() string {
	return fmt.Sprintf(
		"&{APIApp:%s APIKey:[REDACTED] APIURI:%s PrinterName:%s OfficeName:%s}",
		ie.APIApp, ie.APIURI, ie.PrinterName, ie.OfficeName,
	)
}

type Config struct {
	Listen string
	Serial string
	// preserve the typo for backward compatibility
	InternEndpoint *InternEndpoint `json:"InternEnpoint"`
}

// loadConfig reads and validates the daemon configuration.
func loadConfig(path string) (*Config, error) {
	byteValue, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("can't read main config: %w", err)
	}

	config := &Config{}
	if err := json.Unmarshal(byteValue, config); err != nil {
		return nil, fmt.Errorf("can't decode main config: %w", err)
	}

	if config.InternEndpoint == nil {
		return nil, fmt.Errorf("config %q is missing the InternEnpoint section", path)
	}
	if config.InternEndpoint.APIURI == "" {
		return nil, fmt.Errorf("config %q is missing api_uri", path)
	}
	// Every API call sends both credentials, so an empty one fails at
	// runtime on the first request rather than at startup.
	if config.InternEndpoint.APIApp == "" {
		return nil, fmt.Errorf("config %q is missing api_app", path)
	}
	if config.InternEndpoint.APIKey == "" {
		return nil, fmt.Errorf("config %q is missing api_key", path)
	}
	if config.InternEndpoint.PrinterName == "" {
		return nil, fmt.Errorf("config %q is missing printerName", path)
	}
	if config.InternEndpoint.OfficeName == "" {
		return nil, fmt.Errorf("config %q is missing officeName", path)
	}

	if config.Listen == "" {
		config.Listen = defaultListen
	}
	if config.Serial == "" {
		config.Serial = defaultSerial
	}

	return config, nil
}

func main() {
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

	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Fatalf("Failed to log to file %s: %v", logFile, err)
	}
	log.SetOutput(file)
	defer file.Close()

	config, err := loadConfig(configFile)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("config: %v", config.InternEndpoint)

	daemon := &Daemon{
		config: config,
		job:    &juggler.Job{Status: juggler.StatusWaitingJob},
		ie: &InternEndpoint{
			APIApp: config.InternEndpoint.APIApp,
			APIKey: config.InternEndpoint.APIKey,
			APIURI: config.InternEndpoint.APIURI,

			PrinterName: config.InternEndpoint.PrinterName,
			OfficeName:  config.InternEndpoint.OfficeName,
			job:         &juggler.Job{},
		},
	}

	daemon.Start()
}
