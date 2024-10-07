package gcodefeeder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.bug.st/serial"
)

type Status int

const (
	Connecting Status = iota
	ConnectionFail
	FSensorBusy
	Ready
	Printing
	ManuallyPaused
	MMUBusy
	Finished
	Error
)

var strStatus = []string{
	"Connecting",
	"ConnectionFail",
	"FSensorBusy",
	"Ready",
	"Printing",
	"ManuallyPaused",
	"MMUBusy",
	"Finished",
	"Error",
}

func (s Status) String() string {
	if int(s) >= len(strStatus) {
		return strconv.Itoa(int(s))
	}
	return strStatus[s]
}

type Feeder struct {
	deviceName string
	fileName   string
	printerAck chan bool
	progress   int
	status     Status

	tty            serial.Port
	writer         *bufio.Writer
	reader         *bufio.Reader
	progressRegexp *regexp.Regexp

	sync.Mutex
	cancelFunc context.CancelFunc
}

func NewFeeder(deviceName, fileName string) (*Feeder, error) {
	f := Feeder{
		deviceName:     deviceName,
		fileName:       fileName,
		printerAck:     make(chan bool),
		progressRegexp: regexp.MustCompile("M73 P([0-9]+).*"),
	}
	f.status = Connecting
	err := f.connect()
	if err != nil {
		f.status = ConnectionFail
		return nil, fmt.Errorf("failed to connect to %s: %w", deviceName, err)
	}
	f.status = Ready

	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to open %s: %w", fileName, err)
	}

	return &f, nil
}

func (f *Feeder) Cancel() {
	f.Lock()
	defer f.Unlock()
	log.Debug("Feeder: Cancel is called")
	// Feed, read and write function will terminate when context is cancelled
	defer f.cancelFunc()
	instructions := []string{
		//  turn off temperature
		"M104 S0\n",
		// turn off heatbed
		"M140 S0\n",
		// turn off fan
		"M107\n",
	}
	for _, instruction := range instructions {
		_, err := f.writer.Write([]byte(instruction))
		if err != nil {
			log.Errorf("Feeder: Error writing cancellation instructions: %v", err)
		}
	}
	if err := f.writer.Flush(); err != nil {
		log.Errorf("Feeder: Error flushing cancellation instructions: %v", err)
	}

	f.tty.Close()
	f.status = Finished
}

func (f *Feeder) Progress() int {
	return f.progress
}

func (f *Feeder) Status() Status {
	return f.status
}

func (f *Feeder) connect() error {
	mode := &serial.Mode{
		BaudRate: 115200,
	}
	tty, err := serial.Open(f.deviceName, mode)
	if err != nil {
		return err
	}
	f.tty = tty
	return nil
}

func (f *Feeder) read(ctx context.Context) {
	defer f.Cancel()

	seenStart := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
			buf, _, err := f.reader.ReadLine()
			if err != nil {
				log.Errorf("Feeder: Error reading from printer: %v", err)
				f.status = Error
				return
			}
			bufStr := string(buf)

			log.Debug("Feeder: READING: ", bufStr)
			if strings.HasPrefix(bufStr, "ok") && seenStart {
				f.printerAck <- true
			} else if strings.Contains(bufStr, "fsensor") {
				f.status = FSensorBusy
			} else if strings.Contains(bufStr, "MMU") {
				if strings.Contains(bufStr, "DISABLED") {
					continue
				}
				f.status = MMUBusy
			} else if strings.Contains(bufStr, "start") {
				// When serial connection is established:
				// Prusa MK3 returns "start"
				// Prusa MK4 (Firmware Buddy) returns "start"
				// We consider this event as "ready to print"
				//
				// If the first "start" is given - it says printer is ready
				// If the second "start" is given - somebody reset the printer
				if !seenStart {
					time.Sleep(2 * time.Second)
					seenStart = true
					f.printerAck <- true
				} else if seenStart && strings.HasSuffix(bufStr, "start") {
					// This is most likely a reset button press on MK3
					log.Warning("Feeder: Second 'start' sequence")
					return
				}
			}
		}
	}
}

func (f *Feeder) write(ctx context.Context, command string) error {
	r := regexp.MustCompile(";.*")
	rcmd := r.ReplaceAllString(command, "")
	if rcmd == "" {
		return nil
	}

	log.Debug("Feeder: WRITING: ", rcmd)
	_, err := f.writer.Write([]byte(rcmd + "\n"))
	if err != nil {
		return err
	}
	f.writer.Flush()

	if s := f.progressRegexp.ReplaceAllString(rcmd, "$1"); s != rcmd {
		// Ignore errors because not all gcodes have proper progress injected
		f.progress, err = strconv.Atoi(s)
		if err != nil {
			log.Debug("Feeder: Progress parsing error.", err, "Continue...")
		}
	}

	select {
	case <-f.printerAck:
		break
	case <-ctx.Done():
		return errors.New("Context is Done")
	}
	return nil
}

func (f *Feeder) Feed() error {
	defer f.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	f.cancelFunc = cancel
	f.reader = bufio.NewReader(f.tty)
	f.writer = bufio.NewWriter(f.tty)

	go f.read(ctx)

	// Flush whatever junk is in write buffer
	_, _ = f.writer.Write([]byte("\n"))
	// Issue a "firmware buddy" specific command to differentiate between mk3 and mk4
	_, _ = f.writer.Write([]byte("M118 start\n"))
	_ = f.writer.Flush()
	// Be sure we receive initial reset from printer
	<-f.printerAck
	f.Start()

	file, err := os.Open(f.fileName)
	if err != nil {
		f.status = Error
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		for f.status == ManuallyPaused {
			select {
			case <-ctx.Done():
				return errors.New("Context is Done")
			default:
				time.Sleep(5 * time.Second)
				log.Info("Feeder: paused manually")
			}
		}
		f.status = Printing
		err = f.write(ctx, line)
		if err != nil {
			f.status = Error
			return err
		}
	}
	f.status = Finished
	return nil
}

func (f *Feeder) Pause() {
	f.status = ManuallyPaused
}

func (f *Feeder) Start() {
	f.status = Printing
}
