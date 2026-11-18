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
// handle's mu, since a flock does not exclude the handle's own goroutines.
//
// exchangeWait bounds a command's wait for the light. A query, or a write
// that sendLone does not send, that does not get the light in that time sends
// nothing and returns ErrBusy. lockPoll is how often it tries again meanwhile.
// exchangeGap is how long a handle leaves the light free after an exchange
// before its next one, so a handle that sends one command after another, as
// the recorder does when it writes the black pattern, does not keep a polling
// `light-state` out until its wait ends.
//
// The wait is short because the recorder's light calls queue behind each
// other: the controller's watchdog tickle waits for a read-back of the gate's
// writer, and a pause's off write for the controller's own read. With a 2 s
// wait, another open that held the lock kept the tickle from the light past
// its 3 s timeout (review of #79). An exchange takes about 8 ms on the office
// Pi (usbmon), so 50 ms lets another Ghost Wispr command finish first.
var exchangeWait = 50 * time.Millisecond

const (
	lockPoll    = time.Millisecond
	exchangeGap = 3 * lockPoll
)

var featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(&report[0])))
	if errno != 0 {
		return errno
	}
	return nil
}

type blink1 struct {
	file         *os.File
	patternLines uint8
	mu           sync.Mutex // the handle's part of the exchange lock
	released     time.Time  // the end of the handle's last exchange, under mu
}

// OpenBlink1 finds ThingM's blink(1) by its hidraw uevent identity.
func OpenBlink1(sysfsRoot, devRoot string) (Light, error) {
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
		light := &blink1{file: file}
		if err := light.loadVersion(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("read blink(1) version: %w", err)
		}
		return light, nil
	}
	return nil, ErrNoBlink1
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
// watchdog tickle. It waits for the light as send does, and once that wait has
// run out sends the report without the lock. A lone SET has no GET to protect;
// it can only answer a query of the holder, whose reply check then refuses it.
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
// exchange lock. Every command to the blink(1) goes through here. With
// anyway, a lone SET whose wait for the lock ran out is sent without it.
func (b *blink1) exchange(report *[8]byte, reply, anyway bool) error {
	fd := b.file.Fd()
	if err := b.lock(fd); err != nil {
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
	b.released = time.Now()
	b.mu.Unlock()
	return err
}

// lock takes the exchange lock, the handle's mu and then the flock on its
// node, or returns ErrBusy once exchangeWait has passed. It leaves the light
// free for exchangeGap after the handle's last exchange first. It tries before
// it looks at the clock, so a process stopped while it waited still gets a
// free light when it runs again.
func (b *blink1) lock(fd uintptr) error {
	deadline := time.Now().Add(exchangeWait)
	for !b.mu.TryLock() {
		if !time.Now().Before(deadline) {
			return ErrBusy
		}
		time.Sleep(lockPoll)
	}
	if free := exchangeGap - time.Since(b.released); free > 0 {
		time.Sleep(free)
	}
	for {
		err := unix.Flock(int(fd), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			b.mu.Unlock()
			return fmt.Errorf("lock blink(1): %w", err)
		}
		if !time.Now().Before(deadline) {
			b.mu.Unlock()
			return ErrBusy
		}
		time.Sleep(lockPoll)
	}
}

func (b *blink1) SetRGB(r, g, blue uint8) error {
	report := Blink1Report('n', [6]byte{r, g, blue})
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
	report := Blink1Report('D', [6]byte{1, byte(ticks >> 8), byte(ticks), 1, 0, b.flashPatternLines() - 1})
	return b.sendLone(&report)
}

// BlackPattern persists an all-black, non-zero-duration pattern so the
// watchdog turns every LED off after the recorder stops ticking it.
func (b *blink1) BlackPattern() error {
	black, err := b.patternIsBlack()
	if err != nil {
		return err
	}
	if black {
		return nil
	}
	return b.writeBlackPattern()
}

func (b *blink1) flashPatternLines() uint8 {
	return b.patternLines
}

func (b *blink1) patternIsBlack() (bool, error) {
	for line := uint8(0); line < b.flashPatternLines(); line++ {
		report := Blink1Report('R', [6]byte{0, 0, 0, 0, 0, line})
		if err := b.query(&report); err != nil {
			return false, err
		}
		if report[2] != 0 || report[3] != 0 || report[4] != 0 || (report[5] == 0 && report[6] == 0) {
			return false, nil
		}
	}
	return true, nil
}

func (b *blink1) writeBlackPattern() error {
	allLEDs := Blink1Report('l', [6]byte{})
	if err := b.send(&allLEDs); err != nil {
		return err
	}
	for line := uint8(0); line < b.flashPatternLines(); line++ {
		black := Blink1Report('P', [6]byte{0, 0, 0, 0, 10, line})
		if err := b.send(&black); err != nil {
			return err
		}
	}
	save := Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe})
	_ = b.send(&save)
	black, err := b.patternIsBlack()
	if err != nil {
		return err
	}
	if !black {
		return fmt.Errorf("saved blink(1) pattern is not black")
	}
	return nil
}

func (b *blink1) Close() error {
	return b.file.Close()
}
