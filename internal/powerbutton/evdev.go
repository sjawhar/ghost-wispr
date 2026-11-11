package powerbutton

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrNoPowerButton = errors.New("power button not found")

var eventHandler = regexp.MustCompile(`\bevent\d+\b`)

// Linux input-event-codes.h values for a power key press.
const (
	evKey    = 1
	keyPower = 116
)

// FindDevice returns the evdev path for the Raspberry Pi 5 pwr_button device.
func FindDevice(procDevices, devRoot string) (string, error) {
	data, err := os.ReadFile(procDevices)
	if err != nil {
		return "", fmt.Errorf("read input devices: %w", err)
	}
	for _, block := range strings.Split(string(data), "\n\n") {
		if !strings.Contains(block, `N: Name="pwr_button"`) {
			continue
		}
		match := eventHandler.FindString(block)
		if match != "" {
			return filepath.Join(devRoot, match), nil
		}
	}
	return "", ErrNoPowerButton
}

// Event is one power-key state transition from Linux evdev.
type Event struct {
	Down bool
	At   time.Time
}

// ReadEvents decodes power-key events from Linux's 24-byte input_event records.
func ReadEvents(ctx context.Context, r io.Reader, out chan<- Event) error {
	var record [24]byte
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
		if binary.LittleEndian.Uint16(record[16:18]) != evKey || binary.LittleEndian.Uint16(record[18:20]) != keyPower {
			continue
		}
		value := int32(binary.LittleEndian.Uint32(record[20:24]))
		if value != 0 && value != 1 {
			continue
		}
		event := Event{
			Down: value == 1,
			At:   time.Unix(int64(binary.LittleEndian.Uint64(record[0:8])), int64(binary.LittleEndian.Uint64(record[8:16]))*1000).UTC(),
		}
		select {
		case out <- event:
		case <-ctx.Done():
			return nil
		}
	}
}

// Taps turns short down-to-up button presses into one event. A long press is
// left to the Pi PMIC, which powers the board off without Ghost Wispr acting.
func Taps(ctx context.Context, in <-chan Event, maxTap time.Duration, out chan<- struct{}) {
	var downAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-in:
			if !ok {
				return
			}
			if event.Down {
				downAt = event.At
				continue
			}
			if downAt.IsZero() {
				continue
			}
			duration := event.At.Sub(downAt)
			downAt = time.Time{}
			if duration < 0 || duration >= maxTap {
				continue
			}
			select {
			case out <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}
