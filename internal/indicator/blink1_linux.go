//go:build linux && (amd64 || arm64)

package indicator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	hidIOCSetFeature = 0xC0084806
	hidIOCGetFeature = 0xC0084807
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

func (b *blink1) setFeature(report *[8]byte) error {
	return featureIOCTL(b.file.Fd(), hidIOCSetFeature, report)
}

func (b *blink1) getFeature(report *[8]byte) error {
	return featureIOCTL(b.file.Fd(), hidIOCGetFeature, report)
}

// query sends a command report and reads the device's reply into it.
func (b *blink1) query(report *[8]byte) error {
	if err := b.setFeature(report); err != nil {
		return err
	}
	return b.getFeature(report)
}

func (b *blink1) SetRGB(r, g, blue uint8) error {
	report := Blink1Report('n', [6]byte{r, g, blue})
	return b.setFeature(&report)
}

func (b *blink1) ReadRGB() (r, g, blue uint8, err error) {
	report := Blink1Report('r', [6]byte{})
	if err := b.query(&report); err != nil {
		return 0, 0, 0, err
	}
	return report[2], report[3], report[4], nil
}

func (b *blink1) ServerDown(timeout time.Duration) error {
	if timeout <= 0 {
		report := Blink1Report('D', [6]byte{})
		return b.setFeature(&report)
	}
	ticks := timeout / (10 * time.Millisecond)
	if ticks > 0xffff {
		ticks = 0xffff
	}
	report := Blink1Report('D', [6]byte{1, byte(ticks >> 8), byte(ticks), 1, 0, b.flashPatternLines() - 1})
	return b.setFeature(&report)
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
	if err := b.setFeature(&allLEDs); err != nil {
		return err
	}
	for line := uint8(0); line < b.flashPatternLines(); line++ {
		black := Blink1Report('P', [6]byte{0, 0, 0, 0, 10, line})
		if err := b.setFeature(&black); err != nil {
			return err
		}
	}
	save := Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe})
	_ = b.setFeature(&save)
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
