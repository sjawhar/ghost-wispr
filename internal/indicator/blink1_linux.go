//go:build linux && (amd64 || arm64)

package indicator

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

// lockClock is the clock of the exchange lock: a handle's gap before an
// exchange, its deadline and its polls (exchange, flockUntil). The waiters'
// order follows from these waits, and a test that swaps in a clock it drives
// runs them in that order whatever the machine's load, instead of in the
// order a loaded machine happens to end each sleep.
var lockClock clock = systemClock{}

// clock is what the exchange lock reads the time from and sleeps on.
type clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type systemClock struct{}

func (systemClock) Now() time.Time        { return time.Now() }
func (systemClock) Sleep(d time.Duration) { time.Sleep(d) }

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
// this process knows is black (BlackPattern: a save of the black pattern
// reached it) to its pattern line count. A unit opened again, as after a
// device error, is then known black: it needs no version query and no pattern
// check, which another open holding the lock could keep from it, and its
// watchdog can be tickled at once (review 5954543056 of #79). A unit with no
// serial is never known.
var (
	checkedMu    sync.Mutex
	checkedUnits = map[string]uint8{}
)

// PatternKnownBlack reports whether this process knows this unit's stored
// pattern is black, from a check on an earlier handle or this one
// (KnownBlack).
func (b *blink1) PatternKnownBlack() bool {
	if b.serial == "" {
		return false
	}
	checkedMu.Lock()
	defer checkedMu.Unlock()
	_, known := checkedUnits[b.serial]
	return known
}

// saveRecordFresh is how long the records of RecordSavesIn stand. Each check
// of a unit not known black yet ends in a save of the black pattern, and the
// recorder checks its unit at each start: without them, a recorder that failed
// and was restarted every 10 s (ghost-wispr.service: Restart=always,
// RestartSec=10, no start limit) would write the unit's flash about 8,600
// times a day. With them, a unit's flash is written at most once per
// saveRecordFresh, across processes.
const saveRecordFresh = 24 * time.Hour

// The records RecordSavesIn keeps per unit serial number, as file name
// suffixes.
const (
	// attemptRecord: a save was about to be sent. It is written before the
	// save, and spares the flash another for saveRecordFresh, whether the
	// save then reached the unit, missed it, failed or never went out.
	attemptRecord = ".attempt"
	// savedRecord: a save that this process saw reach the unit, of a pattern
	// shown black. Only it makes a later check know the unit black.
	savedRecord = ".saved"
)

var (
	saveRecordMu  sync.Mutex
	saveRecordDir string
)

// RecordSavesIn makes the black pattern check keep two records per unit
// serial number in dir, which must last across restarts. Before it writes the
// unit's flash ('W'), it records the attempt; if it cannot, it writes nothing,
// and the unit is not known black. While an attempt recorded less than
// saveRecordFresh ago stands, no check writes that unit's flash again. Once a
// save is seen to reach the unit, it records the save; while a save recorded
// less than saveRecordFresh ago stands, a check that finds the unit's pattern
// black knows the unit black without a save: Ghost Wispr saves nothing but the
// black pattern, and a save that missed leaves flash as the last save left it.
// An attempt with no save recorded after it, as when the process ended before
// its save went out or before it was confirmed, or the transfer failed,
// leaves the unit unknown, so it is checked whenever it is opened. "" keeps no
// records: every check that needs a save sends one.
func RecordSavesIn(dir string) {
	saveRecordMu.Lock()
	defer saveRecordMu.Unlock()
	saveRecordDir = dir
}

// saveRecordPath is the file of serial's record kind (attemptRecord or
// savedRecord), "" when no record is kept.
func saveRecordPath(serial, kind string) string {
	saveRecordMu.Lock()
	defer saveRecordMu.Unlock()
	if saveRecordDir == "" || serial == "" {
		return ""
	}
	return filepath.Join(saveRecordDir, hex.EncodeToString([]byte(serial))+kind)
}

// claimSave reports whether a save of serial's flash may be sent now: no
// attempt was recorded less than saveRecordFresh ago, and this one is
// recorded first. With no record kept (a unit with no serial number, or no
// RecordSavesIn), it may always be sent.
func claimSave(serial string) bool {
	path := saveRecordPath(serial, attemptRecord)
	if path == "" {
		return true
	}
	now := time.Now()
	if recordedRecently(path, now) {
		return false
	}
	if err := writeSaveRecord(path, now); err != nil {
		slog.Warn("record the blink(1)'s save attempt; the flash is not written, and the unit is checked at each open", "serial", serial, "error", err)
		return false
	}
	return true
}

// recordSave records that a save of serial's black pattern was seen to reach
// it now. A record it cannot write leaves the unit unknown to later processes.
func recordSave(serial string) {
	path := saveRecordPath(serial, savedRecord)
	if path == "" {
		return
	}
	if err := writeSaveRecord(path, time.Now()); err != nil {
		slog.Warn("record the blink(1)'s black pattern save; later processes check the unit", "serial", serial, "error", err)
	}
}

// savedRecently reports whether a save of serial's black pattern was recorded
// as reaching it less than saveRecordFresh before now.
func savedRecently(serial string, now time.Time) bool {
	path := saveRecordPath(serial, savedRecord)
	return path != "" && recordedRecently(path, now)
}

// writeSaveRecord writes the time at to path, through a rename so a reader
// sees the old record or the new one.
func writeSaveRecord(path string, at time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".save-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(at.UTC().Format(time.RFC3339Nano) + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// recordedRecently reports whether the record at path holds a time less than
// saveRecordFresh before now. A time in the future, as after the clock was
// set back, does not count.
func recordedRecently(path string, now time.Time) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	age := now.Sub(at)
	return age >= 0 && age < saveRecordFresh
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
			light.released = lockClock.Now()
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
	return b.exchange(nil, report, false, false)
}

// sendLone sends a lone SET that must reach the light whoever holds it: the
// watchdog tickle, or an off write. It waits for the light as send does, and
// once that wait has run out sends the report without the node's lock. A lone
// SET has no GET to protect; it can only answer a query of another open,
// whose reply check then refuses it. It never goes inside an exchange of its
// own handle (exchange).
func (b *blink1) sendLone(report *[8]byte) error {
	return b.exchange(nil, report, false, true)
}

// query sends a command report and reads the device's reply into it, holding
// the light from the SET to the GET, so no other command's reply answers it.
// Byte 1 of a reply names the command it answers (0172ff00 answers a colour
// read, 01760033 a version query, on the office Pi). A reply that names
// another command came from a program that takes no lock, between this SET and
// GET: query returns ErrOtherReply, and the reply is not used.
func (b *blink1) query(report *[8]byte) error {
	return b.queryAfter(nil, report)
}

// queryAfter is query with first, when not nil, sent before report in the
// same hold of the light: a command whose state report's reply depends on,
// which no other Ghost Wispr command can then change between them. A failed
// first sends nothing more and fails the query.
func (b *blink1) queryAfter(first, report *[8]byte) error {
	command := report[1]
	if err := b.exchange(first, report, true, false); err != nil {
		return err
	}
	if report[1] != command {
		return fmt.Errorf("%w: %q answered %q", ErrOtherReply, report[1], command)
	}
	return nil
}

// exchange sends first, when not nil, then report and, if reply, reads the
// reply into report, all under the exchange lock. Every command to the
// blink(1) goes through here. A command waits for the handle's own exchange
// in flight without a bound, since that exchange is bounded: its wait for the
// node's lock, then its transfers. A bound there made a command of the handle
// report another program (ErrBusy) when only its own pattern check had the
// handle, and sent a lone SET inside that check's query (review 5956273705 of
// #79). With anyway, a lone SET (no first, no reply) whose wait for the node's
// lock ran out is sent without it, still under the handle's mu.
func (b *blink1) exchange(first, report *[8]byte, reply, anyway bool) error {
	fd := b.file.Fd()
	wait, poll := exchangeWait, recorderPoll
	if b.command {
		wait, poll = commandWait, commandPoll
	}
	deadline := lockClock.Now().Add(wait)
	b.mu.Lock()
	defer func() {
		b.released = lockClock.Now()
		b.mu.Unlock()
	}()
	if free := exchangeGap - lockClock.Now().Sub(b.released); free > 0 {
		lockClock.Sleep(free)
	}
	if err := flockUntil(fd, deadline, poll); err != nil {
		if anyway && first == nil && !reply && errors.Is(err, ErrBusy) {
			return featureIOCTL(fd, hidIOCSetFeature, report)
		}
		return err
	}
	var err error
	if first != nil {
		err = featureIOCTL(fd, hidIOCSetFeature, first)
	}
	if err == nil {
		err = featureIOCTL(fd, hidIOCSetFeature, report)
	}
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
		if !lockClock.Now().Before(deadline) {
			return ErrBusy
		}
		lockClock.Sleep(poll)
	}
}

// SetRGB writes every LED's colour ('n', LED 0). An off write reaches the
// light whoever holds it (sendLone), so a pause turns the light off; a red
// write waits for the lock and otherwise sends nothing (ErrBusy). With the
// reply check, an off write's echo, 01 6e 00 00 00, cannot read as red to
// another reader.
func (b *blink1) SetRGB(r, g, blue uint8) error {
	report := Blink1Report('n', [6]byte{r, g, blue})
	if r == 0 && g == 0 && blue == 0 {
		return b.sendLone(&report)
	}
	return b.send(&report)
}

// SetLED2 writes LED 2's colour and leaves LED 1 as it is: a fade ('c') to
// the colour over 1 tick of 10 ms, on LED 2 alone. Set at once ('n') with an
// LED number, the firmware of an mk2 and of an mk3 writes the LED after the
// one it names and starts its pattern player, which fades every LED to the
// stored black pattern (todbot/blink1 hardware/firmware_mk2/main.c,
// todbot/blink1mk3 firmware/firmware-v30x/main.c: the 'n' case sets playing
// to PLAY_DIRECTLED). It waits for the lock as a red write does, and
// otherwise sends nothing (ErrBusy).
func (b *blink1) SetLED2(r, g, blue uint8) error {
	report := Blink1Report('c', [6]byte{r, g, blue, 0, 1, 2})
	return b.send(&report)
}

// ReadRGB reads LED 1's colour back. An mk3's colour read ('r') ignores the
// LED its request names and reads the one the unit's global LED number names
// (todbot/blink1mk3 firmware/firmware-v30x/main.c, tags firmware-v303 and
// firmware-v304: leds[ledn]), which 'l' sets and the pattern player sets from
// each line it plays; so the read first selects LED 1 ('l' 0), in the same
// hold of the light. An mk2 reads the LED byte 7 names, LED 1 here. Any error,
// the select's too, fails the read, and its colour counts as not red.
func (b *blink1) ReadRGB() (r, g, blue uint8, err error) {
	selectLED1 := Blink1Report('l', [6]byte{})
	report := Blink1Report('r', [6]byte{5: 1})
	if err := b.queryAfter(&selectLED1, &report); err != nil {
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
// watchdog turns every LED off after the recorder stops ticking it. Once a
// save of that pattern reaches the unit, the unit is known black
// (checkedUnits). It runs PatternCheck to the end.
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
// writes the black pattern ('l', a 'P' per line), saves it ('W') and reads
// every line again; a pattern that does not read back black fails it. The
// reads see the lines in RAM, which the watchdog plays; 'W' saves them to
// flash, which a power-up loads.
//
// A read of RAM cannot show what flash holds, so a unit is made known black
// (checkedUnits) only by a save of the black pattern seen to reach it: this
// check's, after its read-back, or one recorded as confirmed less than
// saveRecordFresh ago (RecordSavesIn). A check that finds the pattern black on
// a unit with a serial number that is not known yet, and has no such record,
// sends one 'W' to end with. A save that missed earlier, in this check,
// another check, on another handle or in a process that has since ended,
// leaves RAM black and flash lit: only a save that reaches the unit puts it
// right (review 5983673477 of #99). A save that did not reach the unit (another
// open held the lock past its wait, or the transfer failed) still passes the
// check, as the pattern the watchdog plays is black, but the unit is not
// known: plugged in again, it loads the saved pattern and is checked (review
// 5958478401 of #79). Each check sends at most one save, and none while a save
// attempt recorded less than saveRecordFresh ago stands or when it cannot
// record its own (RecordSavesIn); the unit then stays unknown. A read error
// leaves what the check has learned as it was.
func (b *blink1) PatternCheck() PatternCheck { return &blink1PatternCheck{b: b} }

type blink1PatternCheck struct {
	b       *blink1
	lines   uint8 // 0 until the line count is known
	phase   int   // checkRead, checkSelectLEDs, checkWrite or checkSave
	line    uint8 // the next line to read or write
	written bool  // the black pattern is written: the reads check it
	sent    bool  // the check has sent its save, tried to, or may send none
	// saved: a save of the black pattern reached the unit, this check's or
	// one recorded as confirmed less than saveRecordFresh ago.
	saved bool
	done  bool
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
		if !c.sent && c.b.serial != "" && !c.b.PatternKnownBlack() {
			if savedRecently(c.b.serial, time.Now()) {
				c.saved = true
				return c.finish(), nil
			}
			if !claimSave(c.b.serial) {
				// No save now: the unit stays unknown.
				c.sent = true
				return c.finish(), nil
			}
			c.phase = checkSave
			return false, nil
		}
		return c.finish(), nil
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
			c.written = true
			if claimSave(c.b.serial) {
				c.phase = checkSave
			} else {
				// No save now: the read-back shows the written pattern,
				// and the unit stays unknown.
				c.sent, c.phase, c.line = true, checkRead, 0
			}
		}
	case checkSave:
		save := Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe})
		c.sent, c.saved = true, c.b.send(&save) == nil
		if !c.written {
			// The pattern read black just before: the save ends the check.
			return c.finish(), nil
		}
		if c.saved {
			// Flash now holds what the 'P's left in RAM, which the
			// read-back has yet to show black.
			forgetUnit(c.b.serial)
		}
		c.phase, c.line = checkRead, 0
	}
	return false, nil
}

// finish ends the check. If a save of the black pattern reached the unit, it
// makes the unit known black, and records the check's own save.
func (c *blink1PatternCheck) finish() bool {
	if c.b.serial != "" && c.saved {
		checkedMu.Lock()
		checkedUnits[c.b.serial] = c.lines
		checkedMu.Unlock()
		if c.sent {
			recordSave(c.b.serial)
		}
	}
	c.done = true
	return true
}

// forgetUnit makes serial's unit not known black, here and in the record of
// its last confirmed save.
func forgetUnit(serial string) {
	if serial == "" {
		return
	}
	checkedMu.Lock()
	delete(checkedUnits, serial)
	checkedMu.Unlock()
	if path := saveRecordPath(serial, savedRecord); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("drop the blink(1)'s save record", "serial", serial, "error", err)
		}
	}
}

func (b *blink1) Close() error {
	return b.file.Close()
}
