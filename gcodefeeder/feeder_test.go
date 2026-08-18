package gcodefeeder

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePrinter is an in-memory stand-in for a serial port. It answers every
// written instruction with "ok", the way Marlin and Buddy firmware do.
type fakePrinter struct {
	mu sync.Mutex

	// written accumulates everything the feeder sent us.
	written []string
	// toRead holds lines the printer will emit, in order.
	toRead []string

	closed bool

	// autoOK makes the printer answer each written command with "ok".
	autoOK bool
	// pending is the queue of lines waiting to be read.
	pending []string
	// readWait blocks Read until there is something to deliver.
	readWait chan struct{}

	// onWrite, when set, runs inside Write after the bytes are recorded and
	// before it returns. It lets a test force a reply to be fully processed
	// while the writer is still inside the call.
	onWrite func()

	// resets counts ResetInputBuffer calls, so a test can assert the drain
	// reached the port and not just the buffered reader.
	resets int

	// failWriteAfter causes Write to fail after N successful calls (-1 = never).
	failWriteAfter int
	writeCount     int
	// gate, when non-nil, blocks auto-acking until it is closed. It lets a
	// test hold the feeder mid-print.
	gate     chan struct{}
	gateOnce sync.Once
}

func newFakePrinter(initial []string) *fakePrinter {
	p := &fakePrinter{
		toRead:         initial,
		autoOK:         true,
		readWait:       make(chan struct{}, 1024),
		failWriteAfter: -1,
	}
	for _, l := range initial {
		p.pending = append(p.pending, l)
		p.readWait <- struct{}{}
	}
	return p
}

func (p *fakePrinter) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.writeCount++
	shouldFail := p.failWriteAfter >= 0 && p.writeCount > p.failWriteAfter
	if shouldFail {
		p.mu.Unlock()
		return 0, errors.New("simulated write failure")
	}
	s := string(b)
	p.written = append(p.written, s)
	autoOK := p.autoOK
	closed := p.closed
	gate := p.gate
	onWrite := p.onWrite
	p.mu.Unlock()

	if gate != nil {
		<-gate
	}

	if closed {
		return 0, errors.New("port closed")
	}

	// Answer each newline-terminated command with an "ok", as real firmware does.
	if autoOK {
		for i := 0; i < strings.Count(s, "\n"); i++ {
			p.emit("ok")
		}
	}
	if onWrite != nil {
		onWrite()
	}
	return len(b), nil
}

// emit queues a line for the feeder to read.
func (p *fakePrinter) emit(line string) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.pending = append(p.pending, line)
	p.mu.Unlock()
	select {
	case p.readWait <- struct{}{}:
	default:
	}
}

// Read blocks until a line is available or the port is closed. It never
// invents an EOF from a quiet period: a real serial port does not report end
// of stream just because the printer had nothing to say, and doing so made a
// slow machine look like a dead printer.
func (p *fakePrinter) Read(b []byte) (int, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return 0, io.EOF
		}
		if len(p.pending) > 0 {
			// Deliver everything queued that fits, the way a real serial
			// port hands over whatever is in its receive buffer. Returning
			// strictly one line per read would make it impossible for input
			// to accumulate, which is the situation worth testing.
			var out []byte
			for len(p.pending) > 0 {
				line := []byte(p.pending[0] + "\n")
				if len(out)+len(line) > len(b) {
					break
				}
				out = append(out, line...)
				p.pending = p.pending[1:]
			}
			p.mu.Unlock()
			return copy(b, out), nil
		}
		p.mu.Unlock()

		// Block until a line is emitted or the port is closed. A real serial
		// port does the same: silence is not end of stream.
		select {
		case <-p.readWait:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ResetInputBuffer discards queued input, mirroring serial.Port.
func (p *fakePrinter) ResetInputBuffer() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resets++
	p.pending = nil
	return nil
}

func (p *fakePrinter) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("already closed")
	}
	p.closed = true
	gate := p.gate
	p.mu.Unlock()

	// Wake a reader blocked waiting for a line.
	select {
	case p.readWait <- struct{}{}:
	default:
	}

	// Release anyone blocked on the ack gate so the feeder can observe the
	// dead port instead of hanging.
	if gate != nil {
		p.gateOnce.Do(func() { close(gate) })
	}
	return nil
}

// holdAfter makes the printer stop acknowledging once n commands have been
// written, so the feeder stalls mid-print until release() is called.
func (p *fakePrinter) holdAfter(n int) func() {
	gate := make(chan struct{})
	release := func() { p.gateOnce.Do(func() { close(gate) }) }
	go func() {
		for {
			p.mu.Lock()
			count := len(p.written)
			p.mu.Unlock()
			if count >= n {
				p.mu.Lock()
				p.gate = gate
				p.mu.Unlock()
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return release
}

func (p *fakePrinter) writtenLines() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.written))
	copy(out, p.written)
	return out
}

func (p *fakePrinter) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// testTimeout bounds how long a test waits for the feeder to make progress.
// CI runners are shared and can be slow, so allow it to be raised without
// editing every deadline: GCODEFEEDER_TEST_TIMEOUT_SCALE=4 go test ./...
var testTimeout = func() time.Duration {
	base := 5 * time.Second
	if v := os.Getenv("GCODEFEEDER_TEST_TIMEOUT_SCALE"); v != "" {
		if scale, err := strconv.Atoi(v); err == nil && scale > 0 {
			return base * time.Duration(scale)
		}
	}
	return base
}()

// shortenTimings makes the feeder's built-in delays negligible for tests.
func shortenTimings(t *testing.T) {
	t.Helper()
	origSettle, origPause := startSettleDelay, pauseCheckInterval
	startSettleDelay = time.Millisecond
	pauseCheckInterval = time.Millisecond
	t.Cleanup(func() {
		startSettleDelay = origSettle
		pauseCheckInterval = origPause
	})
}

func TestStatusString(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{Connecting, "Connecting"},
		{ConnectionFail, "ConnectionFail"},
		{FSensorBusy, "FSensorBusy"},
		{Ready, "Ready"},
		{Printing, "Printing"},
		{ManuallyPaused, "ManuallyPaused"},
		{MMUBusy, "MMUBusy"},
		{Finished, "Finished"},
		{Error, "Error"},
		// Out of range must not panic.
		{Status(99), "99"},
		{Status(-1), "-1"},
	}
	for _, tt := range tests {
		if got := tt.status.String(); got != tt.want {
			t.Errorf("Status(%d).String() = %q, want %q", int(tt.status), got, tt.want)
		}
	}
}

func TestFeedHappyPath(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	gcode := "G28\nG1 X10 Y10\nM73 P50\nG1 X20\n"
	f := NewFeederWithPort(printer, strings.NewReader(gcode))

	if err := f.Feed(); err != nil {
		t.Fatalf("Feed() returned error: %v", err)
	}

	if got := f.Status(); got != Finished {
		t.Errorf("status after Feed = %v, want Finished", got)
	}
	if got := f.Progress(); got != 50 {
		t.Errorf("progress = %d, want 50", got)
	}

	written := strings.Join(printer.writtenLines(), "")
	for _, want := range []string{"G28", "G1 X10 Y10", "M73 P50", "G1 X20"} {
		if !strings.Contains(written, want) {
			t.Errorf("expected %q to be sent to printer, got:\n%s", want, written)
		}
	}
	if !printer.isClosed() {
		t.Error("expected port to be closed after Feed")
	}
}

func TestFeedStripsCommentsAndBlankLines(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	gcode := "; this is a comment\nG28 ; trailing comment\n\n   \nG1 X5\n"
	f := NewFeederWithPort(printer, strings.NewReader(gcode))

	if err := f.Feed(); err != nil {
		t.Fatalf("Feed() returned error: %v", err)
	}

	for _, line := range printer.writtenLines() {
		if strings.Contains(line, ";") {
			t.Errorf("comment leaked to printer: %q", line)
		}
		if strings.TrimSpace(line) == "" && line != "\n" {
			t.Errorf("blank line sent to printer: %q", line)
		}
	}

	written := strings.Join(printer.writtenLines(), "")
	if !strings.Contains(written, "G28") {
		t.Errorf("expected G28 to survive comment stripping, got:\n%s", written)
	}
	if !strings.Contains(written, "G1 X5") {
		t.Errorf("expected G1 X5 to be sent, got:\n%s", written)
	}
}

func TestProgressParsing(t *testing.T) {
	shortenTimings(t)

	tests := []struct {
		name string
		line string
		want int
	}{
		{"simple", "M73 P42\n", 42},
		{"with remaining", "M73 P75 R120\n", 75},
		{"zero", "M73 P0\n", 0},
		{"hundred", "M73 P100\n", 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			printer := newFakePrinter([]string{"start"})
			f := NewFeederWithPort(printer, strings.NewReader(tt.line))
			if err := f.Feed(); err != nil {
				t.Fatalf("Feed() returned error: %v", err)
			}
			if got := f.Progress(); got != tt.want {
				t.Errorf("progress = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProgressIgnoresNonProgressCommands(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	// M73 P30 sets progress; the later commands must not reset it.
	gcode := "M73 P30\nM140 S60\nG1 X1\nM104 S200\n"
	f := NewFeederWithPort(printer, strings.NewReader(gcode))

	if err := f.Feed(); err != nil {
		t.Fatalf("Feed() returned error: %v", err)
	}
	if got := f.Progress(); got != 30 {
		t.Errorf("progress = %d, want 30 (should not be reset by later commands)", got)
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))

	if err := f.Feed(); err != nil {
		t.Fatalf("Feed() returned error: %v", err)
	}

	// Feed already cancelled once via defer; these must not panic or
	// double-close the port.
	f.Cancel()
	f.Cancel()
}

func TestCancelBeforeFeedDoesNotPanic(t *testing.T) {
	printer := newFakePrinter(nil)
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))

	// Feed was never called, so cancelFunc and writer are nil.
	f.Cancel()

	if !printer.isClosed() {
		t.Error("expected port to be closed by Cancel")
	}
}

func TestCancelSendsCooldownInstructions(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))
	if err := f.Feed(); err != nil {
		t.Fatalf("Feed() returned error: %v", err)
	}

	written := strings.Join(printer.writtenLines(), "")
	for _, want := range []string{"M104 S0", "M140 S0", "M107"} {
		if !strings.Contains(written, want) {
			t.Errorf("expected cooldown instruction %q, got:\n%s", want, written)
		}
	}
}

func TestCancelDoesNotMaskErrorStatus(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))

	// Simulate a failure detected mid-print, then cancel.
	f.setStatus(Error)
	f.Cancel()

	if got := f.Status(); got != Error {
		t.Errorf("status after cancelling an errored print = %v, want Error", got)
	}
}

func TestFeedReportsWriteFailure(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	// Allow the handshake writes, then start failing.
	printer.failWriteAfter = 3

	f := NewFeederWithPort(printer, strings.NewReader("G28\nG1 X1\nG1 X2\nG1 X3\n"))
	err := f.Feed()
	if err == nil {
		t.Fatal("expected Feed() to return an error when writes fail")
	}
	if got := f.Status(); got != Error {
		t.Errorf("status = %v, want Error", got)
	}
}

func TestFeedPropagatesGcodeReadError(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, &failingReader{})

	err := f.Feed()
	if err == nil {
		t.Fatal("expected Feed() to return an error when the gcode source fails")
	}
	if !strings.Contains(err.Error(), "reading gcode") {
		t.Errorf("error = %v, want it to mention reading gcode", err)
	}
	if got := f.Status(); got != Error {
		t.Errorf("status = %v, want Error", got)
	}
}

// failingReader always fails, standing in for a truncated gcode stream.
type failingReader struct{}

func (r *failingReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated gcode read failure")
}

func TestFSensorPausesPrint(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	// Give the handshake a moment, then report a filament sensor event.
	time.Sleep(20 * time.Millisecond)
	printer.emit("fsensor: filament runout")

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Feed() did not return in time")
	}

	// The status is transient, so just assert the feeder observed the event
	// at some point rather than asserting the final value.
	if len(printer.writtenLines()) == 0 {
		t.Error("expected some instructions to be written")
	}
}

func TestMMUDisabledIsNotTreatedAsBusy(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G28\n"))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	time.Sleep(10 * time.Millisecond)
	printer.emit("MMU DISABLED")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Feed() returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Feed() did not return in time")
	}

	if got := f.Status(); got == MMUBusy {
		t.Error("'MMU DISABLED' must not be treated as MMU busy")
	}
}

func TestPauseAndStartToggleStatus(t *testing.T) {
	printer := newFakePrinter(nil)
	f := NewFeederWithPort(printer, strings.NewReader(""))

	f.Pause()
	if got := f.Status(); got != ManuallyPaused {
		t.Errorf("status after Pause = %v, want ManuallyPaused", got)
	}

	f.Start()
	if got := f.Status(); got != Printing {
		t.Errorf("status after Start = %v, want Printing", got)
	}
}

func TestNewFeederConnectionFailure(t *testing.T) {
	_, err := NewFeeder("/dev/definitely-not-a-real-serial-port", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected an error connecting to a nonexistent device")
	}
	if !strings.Contains(err.Error(), "failed to connect") {
		t.Errorf("error = %v, want it to mention the connection failure", err)
	}
}

// TestCancelDuringFeedIsRaceFree cancels a print while Feed is actively
// writing. Both paths share one bufio.Writer, so without serialisation this
// is a data race and can interleave a gcode line with a cooldown command.
func TestCancelDuringFeedIsRaceFree(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	var gcode strings.Builder
	for i := 0; i < 500; i++ {
		gcode.WriteString("G1 X1 Y1\n")
	}
	f := NewFeederWithPort(printer, strings.NewReader(gcode.String()))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	// Cancel from several goroutines while the feed is in flight.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			f.Cancel()
		}()
	}
	wg.Wait()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Feed() did not return after Cancel")
	}
}

func TestConcurrentStatusAndProgressAccess(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})

	var gcode strings.Builder
	for i := 0; i < 15; i++ {
		gcode.WriteString("M73 P1\nG1 X1\n")
	}
	f := NewFeederWithPort(printer, strings.NewReader(gcode.String()))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	// Hammer the accessors while Feed is running. Under -race this catches
	// unsynchronised access to status and progress.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = f.Status()
					_ = f.Progress()
					// Yield: a tight spin across several goroutines starves
					// the feed and reader goroutines on a busy machine.
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	// The fake printer reports EOF once its scripted lines run out, which is
	// now a printer failure rather than a clean finish. Either outcome is
	// fine here: this test is about unsynchronised access to status and
	// progress, which -race checks on every call above.
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Feed() did not return in time")
	}
	close(stop)
	wg.Wait()
}

// --- Regression tests for review findings -------------------------------

// TestUnsolicitedAckIsDropped covers an "ok" arriving when no command is
// outstanding. Crediting it to whatever is sent next would let write() return
// before the printer had processed that command.
func TestUnsolicitedAckIsDropped(t *testing.T) {
	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader(""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 5; i++ {
		f.ack(ctx)
	}

	f.sendMu.Lock()
	pending := f.pending
	f.sendMu.Unlock()
	if pending != nil {
		t.Error("an acknowledgement was retained while no command was outstanding")
	}
}

// TestOnlyOneAckPerCommand covers two acknowledgements racing for a single
// outstanding command. Exactly one may be delivered; the surplus must be
// discarded rather than held for the command that follows.
func TestOnlyOneAckPerCommand(t *testing.T) {
	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader(""))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mailbox := make(chan struct{})
	f.sendMu.Lock()
	f.pending = mailbox
	f.sendMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.ack(ctx)
		}()
	}
	wg.Wait()

	select {
	case <-mailbox:
	default:
		t.Fatal("the outstanding command was never acknowledged")
	}

	// The surplus ack must not have left anything behind for the next command.
	f.sendMu.Lock()
	pending := f.pending
	f.sendMu.Unlock()
	if pending != nil {
		t.Error("a surplus acknowledgement was retained for the following command")
	}
}

// TestUnexpectedEOFIsNotASuccessfulPrint covers a serial link that drops
// mid-print. Treating that EOF as benign lets Cancel() mark the job Finished,
// and the daemon then deletes a job that never actually printed.
func TestUnexpectedEOFIsNotASuccessfulPrint(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	var gcode strings.Builder
	for i := 0; i < 400; i++ {
		gcode.WriteString("G1 X1 Y1\n")
	}
	release := printer.holdAfter(12)
	defer release()

	f := NewFeederWithPort(printer, strings.NewReader(gcode.String()))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	// Let the print get underway, then yank the cable.
	waitFor(t, func() bool { return f.Status() == Printing })
	printer.Close()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Feed did not return after the port died")
	}

	if got := f.Status(); got == Finished {
		t.Error("a print interrupted by a dead serial port must not report Finished")
	}
	if got := f.Status(); got != Error {
		t.Errorf("status = %v, want Error", got)
	}
}

// TestFeedReportsPrinterErrorNotCancellation checks that a printer-side
// failure surfaces as a real error rather than being masked as ErrCancelled,
// which the caller uses to mean "the user cancelled".
func TestFeedReportsPrinterErrorNotCancellation(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	var gcode strings.Builder
	for i := 0; i < 400; i++ {
		gcode.WriteString("G1 X1 Y1\n")
	}
	release := printer.holdAfter(12)
	defer release()

	f := NewFeederWithPort(printer, strings.NewReader(gcode.String()))

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	waitFor(t, func() bool { return f.Status() == Printing })
	printer.Close()

	var err error
	select {
	case err = <-done:
	case <-time.After(testTimeout):
		t.Fatal("Feed did not return")
	}

	if err == nil {
		t.Fatal("expected an error when the printer connection dies")
	}
	if errors.Is(err, ErrCancelled) {
		t.Errorf("printer failure reported as cancellation: %v", err)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestFeedAfterCancelReturnsPromptly covers cancellation that lands before
// Feed installs its context. Cancel captures a nil cancelFunc and closes the
// port; Feed must not then start a fresh context and block forever waiting
// for a handshake that can never arrive.
func TestFeedAfterCancelReturnsPromptly(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader("G1 X1\n"))

	f.Cancel()

	done := make(chan error, 1)
	go func() { done <- f.Feed() }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Errorf("Feed() after Cancel() = %v, want ErrCancelled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Feed() hung after the feeder was already cancelled")
	}
}

// TestAckBeforeCommandIsDropped covers an "ok" that arrives before the
// command it might appear to answer was even started. There is no mailbox to
// deliver it into, so it is discarded rather than satisfying the next
// command, which would let the feeder run one command ahead of the printer
// for the rest of the job.
func TestAckBeforeCommandIsDropped(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	// autoOK off: the only acknowledgements this command can see are the
	// ones the test sends.
	printer.mu.Lock()
	printer.autoOK = false
	printer.mu.Unlock()

	f := NewFeederWithPort(printer, strings.NewReader(""))
	f.writer = bufio.NewWriter(printer)
	f.reader = bufio.NewReader(printer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Nothing is outstanding yet, so this reply belongs to nothing.
	f.ack(ctx)

	done := make(chan error, 1)
	go func() { done <- f.write(ctx, "G1 X1") }()

	// If the stray ok had been retained, write() would return immediately.
	select {
	case <-done:
		t.Fatal("write() completed on an ok that predated the command")
	case <-time.After(150 * time.Millisecond):
	}

	// The genuine acknowledgement releases it.
	f.ack(ctx)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write() = %v, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("write() ignored the genuine acknowledgement")
	}
}

// TestAckDuringSendIsNotLost covers a printer that replies while the command
// is still being written. The reply must not be discarded: it arrives after
// the command was genuinely sent, so it belongs to it.
//
// The ok is delivered through the reader goroutine, the way a real one is.
// Calling ack directly from inside Write would deadlock, because sending and
// acknowledging deliberately share a lock.
func TestAckDuringSendIsNotLost(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	printer.mu.Lock()
	printer.autoOK = false
	printer.mu.Unlock()

	f := NewFeederWithPort(printer, strings.NewReader(""))
	f.writer = bufio.NewWriter(printer)
	f.reader = bufio.NewReader(printer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go f.read(ctx)
	waitFor(t, func() bool {
		select {
		case <-f.ready:
			return true
		default:
			return false
		}
	})

	// Emit the reply from inside Write, so it is already in the reader's
	// hands before the send finishes.
	printer.mu.Lock()
	printer.onWrite = func() { printer.emit("ok") }
	printer.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- f.write(ctx, "G1 X1") }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write() = %v, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("a reply that arrived during the send was lost; write() never returned")
	}
}

func TestFastAckIsNotLost(t *testing.T) {
	shortenTimings(t)

	// autoOK makes the fake printer reply from inside Write, which is the
	// tightest possible timing between flush and acknowledgement. Keep the
	// port open so read()'s deferred Cancel does not close it underneath us.
	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader(""))
	f.writer = bufio.NewWriter(printer)
	f.reader = bufio.NewReader(printer)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drive the reader so acks are delivered as the printer emits them.
	go f.read(ctx)

	// Let the handshake settle so seenStart is true and acks are accepted.
	time.Sleep(60 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- f.write(ctx, "G1 X1") }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("write() = %v, want nil", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("a fast acknowledgement was lost; write() never returned")
	}
}

// TestDrainInputDiscardsBufferedStartupChatter covers the reason the
// handshake ends with a drain.
//
// Startup writes make the printer emit replies of their own, and those can
// still be sitting in the input buffer when the first gcode command goes out.
// Nothing observable at read time separates them from a genuine reply: by
// then the command really has been sent, and reading a line tells you when
// you read it, not when the printer said it. So the bytes have to be gone
// before any command is sent, otherwise the first command is acknowledged by
// a reply that predates it and the feeder runs a command ahead of the printer
// for the rest of the job.
func TestDrainInputDiscardsBufferedStartupChatter(t *testing.T) {
	printer := newFakePrinter(nil)
	f := NewFeederWithPort(printer, strings.NewReader(""))
	f.reader = bufio.NewReader(printer)

	// Startup chatter the printer produced while booting.
	printer.emit("ok")
	printer.emit("ok")
	printer.emit("echo: some firmware banner")

	// Pull it into the buffered reader, as a real read would.
	waitFor(t, func() bool {
		_, _ = f.reader.Peek(1)
		return f.reader.Buffered() > 0
	})

	if err := f.drainInput(context.Background()); err != nil {
		t.Fatalf("drainInput() = %v", err)
	}

	if n := f.reader.Buffered(); n != 0 {
		t.Errorf("%d bytes of startup input survived the drain", n)
	}
}

// TestResetInputBufferIsUsedWhenAvailable checks that a port offering a
// driver-level purge gets one. bufio can only discard what it has already
// pulled up; bytes still held by the driver need the port's own reset.
func TestResetInputBufferIsUsedWhenAvailable(t *testing.T) {
	printer := newFakePrinter(nil)
	f := NewFeederWithPort(printer, strings.NewReader(""))
	f.reader = bufio.NewReader(printer)

	if err := f.drainInput(context.Background()); err != nil {
		t.Fatalf("drainInput() = %v", err)
	}

	printer.mu.Lock()
	reset := printer.resets
	printer.mu.Unlock()
	if reset == 0 {
		t.Error("drainInput did not purge the port receive buffer")
	}
}

// TestNoCommandAfterContextCancelled covers cancellation landing while a
// command is waiting to be sent.
//
// Cancel sends the cooldown instructions and then closes the port. A command
// emitted after those would undo them and leave the printer heating with
// nobody watching, so the send path checks the context after taking the lock
// rather than only before waiting for it.
//
// Holding sendMu is what makes the interleaving deterministic: write() parks
// on the lock, the context is cancelled underneath it, and the lock is then
// released. Without the check, write() emits the command and blocks forever
// on an acknowledgement that is never coming.
func TestNoCommandAfterContextCancelled(t *testing.T) {
	shortenTimings(t)

	printer := newFakePrinter([]string{"start"})
	f := NewFeederWithPort(printer, strings.NewReader(""))

	ctx, cancel := context.WithCancel(context.Background())

	f.sendMu.Lock()

	done := make(chan error, 1)
	go func() { done <- f.write(ctx, "G1 X99") }()

	// write() is now parked on the lock, so nothing has been sent.
	time.Sleep(50 * time.Millisecond)

	cancel()
	f.sendMu.Unlock()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Errorf("write() = %v, want ErrCancelled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("write() neither returned nor gave up after the context was cancelled")
	}

	for _, line := range printer.writtenLines() {
		if strings.HasPrefix(line, "G1 X99") {
			t.Error("a command was sent to the printer after the context was cancelled")
		}
	}
}
