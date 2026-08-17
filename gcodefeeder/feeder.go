package gcodefeeder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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

// ErrCancelled is returned by Feed when the print was cancelled.
var ErrCancelled = errors.New("feed cancelled")

// pauseCheckInterval is how often Feed re-checks whether a manual pause is over.
var pauseCheckInterval = 5 * time.Second

// startSettleDelay gives the printer firmware a moment to finish booting after
// it announces "start" before we send the first instruction.
var startSettleDelay = 2 * time.Second

func (s Status) String() string {
	if s < 0 || int(s) >= len(strStatus) {
		return strconv.Itoa(int(s))
	}
	return strStatus[s]
}

// progressRegexp extracts the percentage from PrusaSlicer M73 progress
// comments, e.g. "M73 P42 R120" -> 42.
var progressRegexp = regexp.MustCompile(`^M73 P([0-9]+)`)

// commentRegexp strips gcode comments, which start at the first ';'.
var commentRegexp = regexp.MustCompile(";.*")

type Feeder struct {
	deviceName string
	gcode      io.Reader

	tty    io.ReadWriteCloser
	writer *bufio.Writer
	reader *bufio.Reader

	// ready is closed once the printer has announced itself with "start".
	// It is a separate signal from the acknowledgement mailbox so the startup
	// handshake can never be mistaken for a reply to a gcode command.
	ready     chan struct{}
	readyOnce sync.Once

	// readErr carries a printer-side failure out to Feed so it can be
	// reported as a real error rather than as a cancellation.
	readErr chan error

	// sendMu serialises sending a command against the reader claiming an
	// acknowledgement for it. It covers the whole transition from "not sent"
	// to "sent, and therefore acknowledgeable", so that:
	//
	//   - an "ok" processed before the command was sent finds nothing
	//     outstanding and is dropped,
	//   - an "ok" that arrives while the command is going out waits behind
	//     this lock and is then delivered, rather than being lost,
	//   - a reply that arrives afterwards is delivered normally.
	//
	// It also guards writer, which is a bufio.Writer and so is not safe for
	// concurrent use: Feed writes gcode through it while Cancel may
	// concurrently write the cooldown instructions.
	sendMu sync.Mutex

	// mu guards progress, status, cancelFunc and cancelled.
	mu         sync.Mutex
	progress   int
	status     Status
	cancelFunc context.CancelFunc
	cancelled  bool
	// pending is the acknowledgement mailbox for the outstanding command, or
	// nil when nothing is awaiting a reply. Guarded by sendMu, so that
	// registering it is part of the same critical section as the write.
	pending chan struct{}
}

// NewFeeder connects to the printer on deviceName and returns a Feeder ready
// to stream gcode to it.
func NewFeeder(deviceName string, gcode io.Reader) (*Feeder, error) {
	f := newFeeder(deviceName, gcode)
	f.setStatus(Connecting)
	tty, err := connect(deviceName)
	if err != nil {
		f.setStatus(ConnectionFail)
		return nil, fmt.Errorf("failed to connect to %s: %w", deviceName, err)
	}
	f.tty = tty
	f.setStatus(Ready)
	return f, nil
}

// NewFeederWithPort returns a Feeder that talks to an already-open port
// instead of opening a serial device. It exists so the feeder can be driven
// against a fake printer in tests.
func NewFeederWithPort(port io.ReadWriteCloser, gcode io.Reader) *Feeder {
	f := newFeeder("", gcode)
	f.tty = port
	f.setStatus(Ready)
	return f
}

func newFeeder(deviceName string, gcode io.Reader) *Feeder {
	return &Feeder{
		deviceName: deviceName,
		gcode:      gcode,
		ready:      make(chan struct{}),
		readErr:    make(chan error, 1),
		status:     Connecting,
	}
}

func connect(deviceName string) (serial.Port, error) {
	return serial.Open(deviceName, &serial.Mode{BaudRate: 115200})
}

// Cancel stops the print, tells the printer to cool down and closes the port.
// It is safe to call multiple times and from multiple goroutines.
func (f *Feeder) Cancel() {
	f.mu.Lock()
	if f.cancelled {
		f.mu.Unlock()
		return
	}
	f.cancelled = true
	cancelFunc := f.cancelFunc
	// Cancelling an errored print must not report it as a clean finish.
	if f.status != Error {
		f.status = Finished
	}
	writer, tty := f.writer, f.tty
	f.mu.Unlock()

	log.Debug("Feeder: Cancel is called")
	// Stop read() and write() before touching the port they are using.
	if cancelFunc != nil {
		cancelFunc()
	}

	// Feed may never have run, in which case there is nothing to write to.
	if writer != nil {
		instructions := []string{
			//  turn off temperature
			"M104 S0\n",
			// turn off heatbed
			"M140 S0\n",
			// turn off fan
			"M107\n",
		}
		f.sendMu.Lock()
		for _, instruction := range instructions {
			if _, err := writer.Write([]byte(instruction)); err != nil {
				log.Errorf("Feeder: Error writing cancellation instructions: %v", err)
			}
		}
		if err := writer.Flush(); err != nil {
			log.Errorf("Feeder: Error flushing cancellation instructions: %v", err)
		}
		f.sendMu.Unlock()
	}

	if tty != nil {
		if err := tty.Close(); err != nil {
			log.Errorf("Feeder: Error closing port: %v", err)
		}
	}
}

func (f *Feeder) Progress() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.progress
}

func (f *Feeder) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *Feeder) setStatus(s Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

func (f *Feeder) setProgress(p int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progress = p
}

func (f *Feeder) isCancelled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelled
}

// ack delivers a printer acknowledgement to the command waiting for one.
//
// The protocol this assumes is strict request/response: after the handshake,
// the firmware emits exactly one "ok" per command it accepts. Marlin and
// Buddy both behave this way, and the feed depends on it, because a bare
// "ok" carries nothing identifying which command it answers.
//
// Replies that cannot belong to an outstanding command are therefore dropped
// rather than credited to the next one. Two sources of those are handled:
// startup chatter, which drainInput discards before the first command is
// sent, and a reply for a command that has already returned, which finds the
// mailbox unclaimed and is discarded here.
//
// A firmware that emitted extra unsolicited "ok" lines mid-print could still
// desynchronise the feed, and no amount of locking would fix that: an
// untagged reply already sitting in the receive queue cannot be attributed
// to one command over another. Correlating them would need the numbered-line
// and checksum protocol (M110 plus N<line> ... *<checksum>), which this
// feeder does not use.
func (f *Feeder) ack(_ context.Context) {
	// Taking sendMu blocks until any in-progress send has finished, so a
	// reply that arrives while the command is going out is still delivered
	// rather than dropped.
	f.sendMu.Lock()
	mailbox := f.pending
	// Claim it. A second "ok" for the same command finds nothing and is
	// discarded, so it cannot be re-delivered to whatever is sent next.
	f.pending = nil
	f.sendMu.Unlock()

	if mailbox == nil {
		log.Debug("Feeder: dropping unsolicited ack")
		return
	}

	// The mailbox is used once and only the claimer closes it, so this never
	// blocks.
	close(mailbox)
}

// drainInput discards whatever the printer has already queued, so that the
// first gcode command cannot be acknowledged by a reply that predates it.
//
// It is called once, at the end of the handshake, before any command is sent.
// Everything buffered at that point is startup chatter by definition.
func (f *Feeder) drainInput(ctx context.Context) error {
	// Purge the driver's receive buffer where the port supports it. bufio
	// may already hold bytes of its own, so the buffered reader is reset too.
	if port, ok := f.tty.(interface{ ResetInputBuffer() error }); ok {
		if err := port.ResetInputBuffer(); err != nil {
			return err
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		n := f.reader.Buffered()
		if n == 0 {
			return nil
		}
		discarded, err := f.reader.Discard(n)
		if err != nil {
			return err
		}
		log.Debugf("Feeder: discarded %d bytes of startup input", discarded)
	}
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
				// A closed port during cancellation is expected. An EOF at
				// any other time means the printer went away mid-print and
				// must not be reported as a clean finish.
				if f.isCancelled() {
					log.Debugf("Feeder: stopped reading from printer: %v", err)
					return
				}
				log.Errorf("Feeder: Error reading from printer: %v", err)
				f.setStatus(Error)
				f.fail(fmt.Errorf("printer connection lost: %w", err))
				return
			}
			bufStr := string(buf)

			log.Debug("Feeder: READING: ", bufStr)
			switch {
			case strings.HasPrefix(bufStr, "ok") && seenStart:
				f.ack(ctx)
			case strings.Contains(bufStr, "fsensor"):
				f.setStatus(FSensorBusy)
			case strings.Contains(bufStr, "MMU"):
				if strings.Contains(bufStr, "DISABLED") {
					continue
				}
				f.setStatus(MMUBusy)
			case strings.Contains(bufStr, "start"):
				// When serial connection is established:
				// Prusa MK3 returns "start"
				// Prusa MK4 (Firmware Buddy) returns "start"
				// We consider this event as "ready to print"
				//
				// If the first "start" is given - it says printer is ready
				// If the second "start" is given - somebody reset the printer
				if !seenStart {
					// Give the firmware a moment to finish booting, then
					// discard anything it queued while doing so.
					//
					// This drain is what makes acknowledgement accounting
					// sound. The handshake writes generate replies of their
					// own, and they can sit unread in the input buffer while
					// we wait here. Without clearing them, the first gcode
					// command would be satisfied by a reply the printer
					// emitted before it ever saw that command, and every
					// acknowledgement afterwards would belong to the previous
					// command. No counter can tell those apart after the
					// fact: reading a line tells you when you read it, not
					// when the printer said it.
					select {
					case <-time.After(startSettleDelay):
					case <-ctx.Done():
						return
					}
					if err := f.drainInput(ctx); err != nil {
						if f.isCancelled() {
							return
						}
						log.Errorf("Feeder: Error draining startup input: %v", err)
						f.setStatus(Error)
						f.fail(fmt.Errorf("printer connection lost: %w", err))
						return
					}
					seenStart = true
					f.readyOnce.Do(func() { close(f.ready) })
				} else if strings.HasSuffix(bufStr, "start") {
					// This is most likely a reset button press on MK3
					log.Warning("Feeder: Second 'start' sequence")
					f.setStatus(Error)
					f.fail(errors.New("printer reset during print"))
					return
				}
			}
		}
	}
}

func (f *Feeder) write(ctx context.Context, command string) error {
	rcmd := commentRegexp.ReplaceAllString(command, "")
	if strings.TrimSpace(rcmd) == "" {
		return nil
	}

	log.Debug("Feeder: WRITING: ", rcmd)

	// Send the command and register its mailbox in one critical section. The
	// command only becomes acknowledgeable once its bytes are out, so a
	// reply the reader picked up beforehand cannot be credited to it. A
	// reply that arrives during the send waits on sendMu inside ack() and is
	// delivered as soon as the mailbox exists, so it is not lost either.
	mailbox := make(chan struct{})

	f.sendMu.Lock()
	// Cancel may have run while we waited for the lock, in which case it has
	// already sent the cooldown instructions. Emitting more gcode now would
	// undo them and keep the printer heating.
	select {
	case <-ctx.Done():
		f.sendMu.Unlock()
		return ErrCancelled
	default:
	}
	_, werr := f.writer.Write([]byte(rcmd + "\n"))
	if werr == nil {
		werr = f.writer.Flush()
	}
	if werr == nil {
		f.pending = mailbox
	}
	f.sendMu.Unlock()
	if werr != nil {
		return fmt.Errorf("writing to printer: %w", werr)
	}

	// Retire the mailbox on the way out so a later stray "ok" cannot be
	// credited to a command that has already returned.
	defer func() {
		f.sendMu.Lock()
		if f.pending == mailbox {
			f.pending = nil
		}
		f.sendMu.Unlock()
	}()

	if m := progressRegexp.FindStringSubmatch(rcmd); m != nil {
		// Ignore errors because not all gcodes have proper progress injected
		if p, err := strconv.Atoi(m[1]); err == nil {
			f.setProgress(p)
		} else {
			log.Debug("Feeder: Progress parsing error.", err, "Continue...")
		}
	}

	select {
	case <-mailbox:
	case <-ctx.Done():
		return ErrCancelled
	}
	return nil
}

// fail records the first printer-side error so Feed can return it.
func (f *Feeder) fail(err error) {
	select {
	case f.readErr <- err:
	default:
	}
}

// printerErr returns a printer-side failure if one was recorded.
func (f *Feeder) printerErr() error {
	select {
	case err := <-f.readErr:
		return err
	default:
		return nil
	}
}

// Feed streams the gcode to the printer, one instruction per acknowledgement,
// and returns when the job is done, cancelled or fails.
func (f *Feeder) Feed() error {
	defer f.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	// Cancel may already have run, in which case it captured a nil
	// cancelFunc and closed the port. Starting a fresh context here would
	// leave nobody to cancel it and Feed would block on a handshake that can
	// never complete.
	if f.cancelled {
		f.mu.Unlock()
		cancel()
		return ErrCancelled
	}
	f.cancelFunc = cancel
	f.reader = bufio.NewReader(f.tty)
	f.writer = bufio.NewWriter(f.tty)
	writer := f.writer
	f.mu.Unlock()

	go f.read(ctx)

	// Flush whatever junk is in write buffer
	f.sendMu.Lock()
	_, hsErr := writer.Write([]byte("\n"))
	if hsErr == nil {
		// Issue a "firmware buddy" specific command to differentiate between mk3 and mk4
		_, hsErr = writer.Write([]byte("M118 start\n"))
	}
	if hsErr == nil {
		hsErr = writer.Flush()
	}
	f.sendMu.Unlock()
	if hsErr != nil {
		f.setStatus(Error)
		return fmt.Errorf("printer handshake failed: %w", hsErr)
	}
	// Be sure we receive initial reset from printer
	select {
	case <-f.ready:
	case <-ctx.Done():
		if err := f.printerErr(); err != nil {
			return err
		}
		return ErrCancelled
	}
	f.Start()

	scanner := bufio.NewScanner(f.gcode)
	for scanner.Scan() {
		line := scanner.Text()
		for f.Status() == ManuallyPaused {
			select {
			case <-ctx.Done():
				if err := f.printerErr(); err != nil {
					return err
				}
				return ErrCancelled
			case <-time.After(pauseCheckInterval):
				log.Info("Feeder: paused manually")
			}
		}
		select {
		case <-ctx.Done():
			if err := f.printerErr(); err != nil {
				return err
			}
			return ErrCancelled
		default:
		}
		f.setStatus(Printing)
		if err := f.write(ctx, line); err != nil {
			// A cancelled context can mean either a user cancel or a printer
			// failure that tore the context down. Prefer the real cause.
			if errors.Is(err, ErrCancelled) {
				if perr := f.printerErr(); perr != nil {
					return perr
				}
				return err
			}
			f.setStatus(Error)
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		f.setStatus(Error)
		return fmt.Errorf("reading gcode: %w", err)
	}
	f.setStatus(Finished)
	return nil
}

func (f *Feeder) Pause() {
	f.setStatus(ManuallyPaused)
}

func (f *Feeder) Start() {
	f.setStatus(Printing)
}
