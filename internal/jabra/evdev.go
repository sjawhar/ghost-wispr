// Package jabra reads the mute button of a USB Jabra speakerphone and sets its
// mute light through the Linux evdev node the kernel's HID driver makes for
// the device's telephony controls: the HID Telephony usage Phone Mute arrives
// as KEY_MICMUTE, and the HID LED usage Mute is the input light LED_MUTE.
// The light is written on that node rather than through /sys/class/leds, so
// one device node, and one udev rule, give the service both.
package jabra

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var ErrNoMuteButton = errors.New("jabra mute button not found")

// ErrNoMonotonicClock is StampWithMonotonicClock's error: the node did not
// take the monotonic clock.
var ErrNoMonotonicClock = errors.New("the input node refused the monotonic clock")

var eventHandler = regexp.MustCompile(`\bevent\d+\b`)

// Linux input-event-codes.h values, and the size of a 64-bit input_event.
const (
	eventSize  = 24
	evSyn      = 0x00
	evKey      = 0x01
	evLed      = 0x11
	synReport  = 0
	keyMicMute = 248
	ledMute    = 0x07
)

// evIocSClockID is linux/input.h's EVIOCSCLOCKID, _IOW('E', 0xa0, int): it
// sets the clock with which an evdev node stamps the events one reader reads.
const evIocSClockID = 0x400445a0

// StampWithMonotonicClock makes the kernel stamp the events read from f with
// CLOCK_MONOTONIC instead of CLOCK_REALTIME, so a step of the wall clock, as
// when the Pi sets its time at boot, moves no stamp. The ioctl goes through
// SyscallConn, so f stays non-blocking and Close still ends a blocked Read.
func StampWithMonotonicClock(f *os.File) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoMonotonicClock, err)
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetPointerInt(int(fd), evIocSClockID, unix.CLOCK_MONOTONIC)
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrNoMonotonicClock, err)
	}
	if ioctlErr != nil {
		return fmt.Errorf("%w: %w", ErrNoMonotonicClock, ioctlErr)
	}
	return nil
}

// MonotonicNow reads CLOCK_MONOTONIC, the clock StampWithMonotonicClock gives
// a node's events.
func MonotonicNow() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(fmt.Sprintf("read CLOCK_MONOTONIC: %v", err))
	}
	return time.Duration(ts.Nano())
}

// FindDevice returns the evdev path of the Jabra input device that has both
// the KEY_MICMUTE key and the LED_MUTE light. A Jabra speakerphone makes
// several input devices; only its telephony device has both.
func FindDevice(procDevices, devRoot string) (string, error) {
	data, err := os.ReadFile(procDevices)
	if err != nil {
		return "", fmt.Errorf("read input devices: %w", err)
	}
	for block := range strings.SplitSeq(string(data), "\n\n") {
		if !strings.Contains(block, `N: Name="Jabra `) ||
			!hasCapability(block, "B: KEY=", keyMicMute) ||
			!hasCapability(block, "B: LED=", ledMute) {
			continue
		}
		if match := eventHandler.FindString(block); match != "" {
			return filepath.Join(devRoot, match), nil
		}
	}
	return "", ErrNoMuteButton
}

// hasCapability reports whether a /proc/bus/input/devices bitmap line has a
// bit set. The kernel prints the bitmap as hexadecimal unsigned longs, the
// most significant first, and leaves out leading zero longs.
func hasCapability(block, prefix string, bit int) bool {
	for line := range strings.SplitSeq(block, "\n") {
		value, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		words := strings.Fields(value)
		index := len(words) - 1 - bit/bits.UintSize
		if index < 0 {
			return false
		}
		word, err := strconv.ParseUint(words[index], 16, 64)
		return err == nil && word>>(bit%bits.UintSize)&1 == 1
	}
	return false
}

// ReadPresses sends the kernel's time stamp of each press of the mute button
// on out, as a reading of the node's clock: CLOCK_MONOTONIC once
// StampWithMonotonicClock has set it, comparable with MonotonicNow. The key's
// release, a repeat, other keys and the light's own events are skipped.
func ReadPresses(ctx context.Context, r io.Reader, out chan<- time.Duration) error {
	var record [eventSize]byte
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if _, err := io.ReadFull(r, record[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read input event: %w", err)
		}
		if binary.LittleEndian.Uint16(record[16:18]) != evKey ||
			binary.LittleEndian.Uint16(record[18:20]) != keyMicMute ||
			int32(binary.LittleEndian.Uint32(record[20:24])) != 1 {
			continue
		}
		at := time.Duration(binary.LittleEndian.Uint64(record[0:8]))*time.Second +
			time.Duration(binary.LittleEndian.Uint64(record[8:16]))*time.Microsecond
		select {
		case out <- at:
		case <-ctx.Done():
			return nil
		}
	}
}

// ShowMuteLight sets the mute light. The input core passes a light event to
// the device only when it differs from the kernel's copy of the light
// (drivers/input/input.c, input_get_disposition), and the HID driver then
// sends the device the light's whole output report.
func ShowMuteLight(w io.Writer, on bool) error {
	return writeEvents(w, appendLight(nil, on))
}

// ResendMuteLight sets the mute light even when the kernel's copy already
// holds that state, as after the device changed its own light: it sets the
// other state first, in the same write, so the kernel's copy changes and the
// device is sent the light again. The last report the device gets carries on:
// the HID driver builds each report from the field's latest value.
func ResendMuteLight(w io.Writer, on bool) error {
	return writeEvents(w, appendLight(appendLight(nil, !on), on))
}

func appendLight(buf []byte, on bool) []byte {
	buf = appendEvent(buf, evLed, ledMute, boolValue(on))
	return appendEvent(buf, evSyn, synReport, 0)
}

// appendEvent appends one input_event with a zero time; the kernel stamps
// events written to an evdev node itself.
func appendEvent(buf []byte, eventType, code uint16, value int32) []byte {
	var record [eventSize]byte
	binary.LittleEndian.PutUint16(record[16:18], eventType)
	binary.LittleEndian.PutUint16(record[18:20], code)
	binary.LittleEndian.PutUint32(record[20:24], uint32(value))
	return append(buf, record[:]...)
}

// writeEvents writes all events in one write, so the kernel applies them in
// order. An io.Writer returns an error for any short write.
func writeEvents(w io.Writer, events []byte) error {
	if _, err := w.Write(events); err != nil {
		return fmt.Errorf("write mute light: %w", err)
	}
	return nil
}

func boolValue(on bool) int32 {
	if on {
		return 1
	}
	return 0
}
