package gcodefeeder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"go.bug.st/serial.v1"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
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

	cancelFunc context.CancelFunc
}

func NewFeeder(deviceName, fileName string) (*Feeder, error) {
	f := Feeder{
		deviceName:     deviceName,
		fileName:       fileName,
		printerAck:     make(chan bool, 0),
		progressRegexp: regexp.MustCompile("M73 P([0-9]+).*"),
	}
	f.status = Connecting
	err := f.connect()
	if err != nil {
		f.status = ConnectionFail
		return nil, errors.New(fmt.Sprintf("failed to connect to %s: %v", deviceName, err))
	}
	f.status = Ready

	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return nil, errors.New(fmt.Sprintf("failed to open %s: %v", fileName, err))
	}

	return &f, nil
}

func (f *Feeder) Cancel() {
	f.cancelFunc()
	//  turn off temperature
	f.writer.Write([]byte("M104 S0\n"))
	// turn off heatbed
	f.writer.Write([]byte("M140 S0\n"))
	// turn off fan
	f.writer.Write([]byte("M107\n"))
	f.writer.Flush()

	f.tty.Close()
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
	defer f.cancelFunc()

	seenStart := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
			buf, _, err := f.reader.ReadLine()
			if err != nil {
				return
			}
			bufStr := string(buf)

			log.Debug("READING:", bufStr)
			if strings.HasPrefix(bufStr, "ok") && seenStart {
				f.printerAck <- true
			} else if strings.Contains(bufStr, "fsensor") {
				f.status = FSensorBusy
			} else if strings.Contains(bufStr, "MMU") {
				f.status = MMUBusy
			} else if strings.Contains(bufStr, "start") {
				// Often "start" comes from MMU, filament sensor etc:
				// msg="READING:MMU => 'start'"
				// msg="READING:fsensor_oq_meassure_start"
				// But the "important" initial start always comes with nothing extra.
				// Though sometimes it has an old junk (thanks buffered I/O):
				// msg="READING:\x1bstart"
				// But still as "Suffix"
				//
				// If the first "start" is given - it says printer is ready
				// If the second "start" is given - somebody reset the printer
				if !seenStart {
					time.Sleep(2 * time.Second)
					seenStart = true
					f.printerAck <- true
				} else if seenStart && strings.HasSuffix(bufStr, "start") {
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

	log.Debug("WRITING:", rcmd)
	_, err := f.writer.Write([]byte(rcmd + "\n"))
	if err != nil {
		return err
	}
	f.writer.Flush()

	if s := f.progressRegexp.ReplaceAllString(rcmd, "$1"); s != rcmd {
		// Ignore errors because not all gcodes have proper progress injected
		f.progress, err = strconv.Atoi(s)
		if err != nil {
			log.Debug("Progress parsing error.", err, "Continue...")
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
	f.writer.Write([]byte("\n"))
	f.writer.Flush()
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
				log.Info("Feeder is paused manually")
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
