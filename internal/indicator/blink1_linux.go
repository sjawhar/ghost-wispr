//go:build linux && (amd64 || arm64)

package indicator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	hidIOCSetFeature = 0xC0084806
	hidIOCGetFeature = 0xC0084807
)

// A blink(1) has one feature-report buffer, which every open handle shares: a
// GET_FEATURE returns what the last SET_FEATURE left in it, whoever sent that
// SET. So each command holds the light for its whole exchange, its SET and,
// for a query, the GET of the reply: flock(LOCK_EX) on the hidraw node, which
// every open of the node contends for, in this process or another (the
// recorder, `ghost-wispr light-state`, `ghost-wispr light-off`); and the
// handle's mu, since a flock does not exclude the handle's own goroutines. A
// command waits for mu without a bound (exchange); only the flock's wait is
// bounded.
//
// exchangeWait bounds the recorder's wait for the node's lock, and commandWait
// that of a subcommand (`light-state`, `light-off`). A query, or a write that
// sendLone does not send, that does not get the lock in that time sends
// nothing and returns ErrBusy. exchangeGap is how long a handle leaves the
// light free after an exchange before its next one, so a handle that sends one
// command after another, as the recorder does when it writes the black
// pattern, does not keep another open out until its wait ends.
//
// The recorder's wait is short because its light calls queue behind each
// other: the controller's watchdog tickle waits for a read-back of the gate's
// writer, and a pause's off write for the controller's own read. With a 2 s
// wait, another open that held the lock kept the tickle from the light past
// its 3 s timeout (review of #79). An exchange takes about 8 ms on the office
// Pi (usbmon), so 50 ms lets another Ghost Wispr command finish first.
//
// The flock serves its waiters in no order, so a waiting recorder is made to
// go first: it tries again every recorderPoll, a subcommand only every
// commandPoll, and a subcommand leaves the light free for exchangeGap before
// its first exchange too. Polling as often as the recorder, back-to-back
// light-state runs took the light from it until its wait ran out: with 4
// light-state loops on a uhid blink(1), the recorder dropped 18 buffers of
// audio in 20 s (review of #79). A subcommand can wait the longer commandWait.
// recorderPoll is 1 ms because a shorter sleep sleeps 1 ms anyway: Go's
// runtime waits for a timer in whole milliseconds (netpoll_epoll.go, go1.25).
var (
	exchangeWait = 50 * time.Millisecond
	commandWait  = 2 * time.Second
)

const (
	recorderPoll = time.Millisecond
	commandPoll  = 5 * time.Millisecond
	exchangeGap  = 3 * time.Millisecond
)

var featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(&report[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

type blink1 struct {
	file *os.File
	// serial is the unit's USB serial number (HID_UNIQ), "" when it reports none.
	serial string
	// command marks a subcommand's handle: it waits commandWait, polling every
	// commandPoll, so a waiting recorder goes first.
	command  bool
	mu       sync.Mutex // the handle's part of the exchange lock; waited for without a bound
	released time.Time  // the end of the handle's last exchange, under mu

	versionMu sync.Mutex
	// patternLines is how many pattern lines the light's flash holds, from its
	// version; 0 until it is read (lines).
	patternLines uint8
}

// checkedUnits maps the serial number of each blink(1) whose stored pattern
// this process found black (BlackPattern) to its pattern line count. A unit
// opened again, as after a device error, is then known black: it needs no
// version query and no pattern check, which another open holding the lock
// could keep from it, and its watchdog can be tickled at once (review
// 5954543056 of #79). A unit with no serial is never known.
var (
	checkedMu    sync.Mutex
	checkedUnits = map[string]uint8{}
)

// PatternKnownBlack reports whether this process found this unit's stored
// pattern black on an earlier handle or this one (KnownBlack).
func (b *blink1) PatternKnownBlack() bool {
	if b.serial == "" {
		return false
	}
	checkedMu.Lock()
	defer checkedMu.Unlock()
	_, known := checkedUnits[b.serial]
	return known
}

// uniq returns the HID_UNIQ value of a hidraw device's uevent, "" when it has
// none.
func uniq(uevent string) string {
	for line := range strings.SplitSeq(uevent, "\n") {
		if value, ok := strings.CutPrefix(line, "HID_UNIQ="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// OpenBlink1 finds ThingM's blink(1) by its hidraw uevent identity and opens
// it for the recorder.
func OpenBlink1(sysfsRoot, devRoot string) (Light, error) {
	return openBlink1(sysfsRoot, devRoot, false)
}

// OpenBlink1ForCommand opens the blink(1) for a subcommand, `light-state` or
// `light-off`: its commands wait for the light longer than the recorder's,
// and let a waiting recorder go first.
func OpenBlink1ForCommand(sysfsRoot, devRoot string) (Light, error) {
	return openBlink1(sysfsRoot, devRoot, true)
}

// openBlink1 opens the blink(1) and reads its version. When another command
// has the light, the version waits until a command needs it (lines), so an
// open, and with it `light-off`'s off write, does not fail because another
// open holds the lock (review of #79).
func openBlink1(sysfsRoot, devRoot string, command bool) (Light, error) {
	hidrawDir := filepath.Join(sysfsRoot, "class", "hidraw")
	entries, err := os.ReadDir(hidrawDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoBlink1
		}
		return nil, fmt.Errorf("read hidraw devices: %w", err)
	}
	for _, entry := range entries {
		uevent, err := os.ReadFile(filepath.Join(hidrawDir, entry.Name(), "device", "uevent"))
		if err != nil {
			continue
		}
		if !strings.Contains(string(uevent), "HID_ID=0003:000027B8:000001ED") {
			continue
		}
		file, err := os.OpenFile(filepath.Join(devRoot, entry.Name()), os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open blink(1) %s: %w", entry.Name(), err)
		}
		light := &blink1{file: file, serial: uniq(string(uevent)), command: command}
		if command {
			light.released = time.Now()
		}
		if light.serial != "" {
			checkedMu.Lock()
			light.patternLines = checkedUnits[light.serial]
			checkedMu.Unlock()
		}
		if _, err := light.lines(); err != nil && !Contended(err) {
			_ = file.Close()
			return nil, err
		}
		return light, nil
	}
	return nil, ErrNoBlink1
}

// lines returns how many pattern lines the light's flash holds, reading its
// version first if no command has yet.
func (b *blink1) lines() (uint8, error) {
	b.versionMu.Lock()
	defer b.versionMu.Unlock()
	if b.patternLines == 0 {
		if err := b.loadVersion(); err != nil {
			return 0, fmt.Errorf("read blink(1) version: %w", err)
		}
	}
	return b.patternLines, nil
}

func (b *blink1) loadVersion() error {
	report := Blink1Report('v', [6]byte{})
	if err := b.query(&report); err != nil {
		return err
	}
	version := report[3]
	if version >= '0' && version <= '9' {
		version -= '0'
	}
	if version < 2 {
		b.patternLines = 12
	} else {
		b.patternLines = 32
	}
	return nil
}

// send sends a command report, holding the light while it does.
func (b *blink1) send(report *[8]byte) error {
	return b.exchange(report, false, false)
}

// sendLone sends a lone SET that must reach the light whoever holds it: the
// watchdog tickle, or an off write. It waits for the light as send does, and
// once that wait has run out sends the report without the node's lock. A lone
// SET has no GET to protect; it can only answer a query of another open,
// whose reply check then refuses it. It never goes inside an exchange of its
// own handle (exchange).
func (b *blink1) sendLone(report *[8]byte) error {
	return b.exchange(report, false, true)
}

// query sends a command report and reads the device's reply into it, holding
// the light from the SET to the GET, so no other command's reply answers it.
// Byte 1 of a reply names the command it answers (0172ff00 answers a colour
// read, 01760033 a version query, on the office Pi). A reply that names
// another command came from a program that takes no lock, between this SET and
// GET: query returns ErrOtherReply, and the reply is not used.
func (b *blink1) query(report *[8]byte) error {
	command := report[1]
	if err := b.exchange(report, true, false); err != nil {
		return err
	}
	if report[1] != command {
		return fmt.Errorf("%w: %q answered %q", ErrOtherReply, report[1], command)
	}
	return nil
}

// exchange sends report and, if reply, reads the reply into it, all under the
// exchange lock. Every command to the blink(1) goes through here. A command
// waits for the handle's own exchange in flight without a bound, since that
// exchange is bounded: its wait for the node's lock, then two transfers. A
// bound there made a command of the handle report another program (ErrBusy)
// when only its own pattern check had the handle, and sent a lone SET inside
// that check's query (review 5956273705 of #79). With anyway, a lone SET whose
// wait for the node's lock ran out is sent without it, still under the
// handle's mu.
func (b *blink1) exchange(report *[8]byte, reply, anyway bool) error {
	fd := b.file.Fd()
	wait, poll := exchangeWait, recorderPoll
	if b.command {
		wait, poll = commandWait, commandPoll
	}
	deadline := time.Now().Add(wait)
	b.mu.Lock()
	defer func() {
		b.released = time.Now()
		b.mu.Unlock()
	}()
	if free := exchangeGap - time.Since(b.released); free > 0 {
		time.Sleep(free)
	}
	if err := flockUntil(fd, deadline, poll); err != nil {
		if anyway && !reply && errors.Is(err, ErrBusy) {
			return featureIOCTL(fd, hidIOCSetFeature, report)
		}
		return err
	}
	err := featureIOCTL(fd, hidIOCSetFeature, report)
	if err == nil && reply {
		err = featureIOCTL(fd, hidIOCGetFeature, report)
	}
	if unlockErr := unix.Flock(int(fd), unix.LOCK_UN); unlockErr != nil && err == nil {
		err = fmt.Errorf("unlock blink(1): %w", unlockErr)
	}
	return err
}

// flockUntil takes the flock on the node, trying every poll, or returns
// ErrBusy once deadline has passed: another open of the node held it. It
// tries before it looks at the clock, so a process stopped while it waited
// still gets a free light when it runs again.
func flockUntil(fd uintptr, deadline time.Time, poll time.Duration) error {
	for {
		err := unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("lock blink(1): %w", err)
		}
		if !time.Now().Before(deadline) {
			return ErrBusy
		}
		time.Sleep(poll)
	}
}

// SetRGB writes the light's colour. An off write reaches the light whoever
// holds it (sendLone), so a pause turns the light off; a red write waits for
// the lock and otherwise sends nothing (ErrBusy). With the reply check, an
// off write's echo, 01 6e 00 00 00, cannot read as red to another reader.
func (b *blink1) SetRGB(r, g, blue uint8) error {
	report := Blink1Report('n', [6]byte{r, g, blue})
	if r == 0 && g == 0 && blue == 0 {
		return b.sendLone(&report)
	}
	return b.send(&report)
}

func (b *blink1) ReadRGB() (r, g, blue uint8, err error) {
	report := Blink1Report('r', [6]byte{})
	if err := b.query(&report); err != nil {
		return 0, 0, 0, err
	}
	return report[2], report[3], report[4], nil
}

// ServerDown tickles the blink(1)'s watchdog, or with timeout 0 turns it off.
// It reaches the light whoever holds it (sendLone), so the watchdog does not
// fire while the recorder runs.
func (b *blink1) ServerDown(timeout time.Duration) error {
	if timeout <= 0 {
		report := Blink1Report('D', [6]byte{})
		return b.sendLone(&report)
	}
	ticks := timeout / (10 * time.Millisecond)
	if ticks > 0xffff {
		ticks = 0xffff
	}
	lines, err := b.lines()
	if err != nil {
		return err
	}
	report := Blink1Report('D', [6]byte{1, byte(ticks >> 8), byte(ticks), 1, 0, lines - 1})
	return b.sendLone(&report)
}

// BlackPattern persists an all-black, non-zero-duration pattern so the
// watchdog turns every LED off after the recorder stops ticking it. Once the
// pattern reads back black, the unit is known black (checkedUnits). It runs
// PatternCheck to the end.
func (b *blink1) BlackPattern() error {
	check := b.PatternCheck()
	for {
		done, err := check.Step()
		if err != nil || done {
			return err
		}
	}
}

// PatternCheck returns the black pattern check of BlackPattern, which sends
// one command per Step: the version query if the line count is not known,
// then a query of each pattern line. At the first line that is not black, it
// writes the black pattern ('l', a 'P' per line, 'W') and reads every line
// again; a pattern that does not read back black fails it. The reads see the
// lines in RAM, which the watchdog plays; 'W' saves them to flash, which a
// power-up loads. A save that did not reach the unit (another open held the
// lock past its wait, or the transfer failed) still passes the check, as
// before, but does not make the unit known black (checkedUnits): plugged in
// again, it loads its old pattern and must be checked (review 5958478401 of
// #79).
func (b *blink1) PatternCheck() PatternCheck { return &blink1PatternCheck{b: b} }

type blink1PatternCheck struct {
	b       *blink1
	lines   uint8 // 0 until the line count is known
	phase   int   // checkRead, checkSelectLEDs, checkWrite or checkSave
	line    uint8 // the next line to read or write
	written bool  // the black pattern is written: the reads check it
	unsaved bool  // the written pattern's save did not reach the unit
	done    bool
}

const (
	checkRead = iota
	checkSelectLEDs
	checkWrite
	checkSave
)

func (c *blink1PatternCheck) Step() (bool, error) {
	if c.done {
		return true, nil
	}
	if c.lines == 0 {
		c.b.versionMu.Lock()
		known := c.b.patternLines
		c.b.versionMu.Unlock()
		if known == 0 {
			lines, err := c.b.lines() // the version query: this step's command
			if err != nil {
				return false, err
			}
			c.lines = lines
			return false, nil
		}
		c.lines = known
	}
	switch c.phase {
	case checkRead:
		report := Blink1Report('R', [6]byte{0, 0, 0, 0, 0, c.line})
		if err := c.b.query(&report); err != nil {
			return false, err
		}
		if report[2] != 0 || report[3] != 0 || report[4] != 0 || (report[5] == 0 && report[6] == 0) {
			if c.written {
				return false, errors.New("saved blink(1) pattern is not black")
			}
			c.phase, c.line = checkSelectLEDs, 0
			return false, nil
		}
		if c.line++; c.line < c.lines {
			return false, nil
		}
		if c.b.serial != "" && !c.unsaved {
			checkedMu.Lock()
			checkedUnits[c.b.serial] = c.lines
			checkedMu.Unlock()
		}
		c.done = true
		return true, nil
	case checkSelectLEDs:
		allLEDs := Blink1Report('l', [6]byte{})
		if err := c.b.send(&allLEDs); err != nil {
			return false, err
		}
		c.phase = checkWrite
	case checkWrite:
		black := Blink1Report('P', [6]byte{0, 0, 0, 0, 10, c.line})
		if err := c.b.send(&black); err != nil {
			return false, err
		}
		if c.line++; c.line == c.lines {
			c.phase = checkSave
		}
	case checkSave:
		save := Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe})
		c.unsaved = c.b.send(&save) != nil
		c.phase, c.line, c.written = checkRead, 0, true
	}
	return false, nil
}

func (b *blink1) Close() error {
	return b.file.Close()
}
