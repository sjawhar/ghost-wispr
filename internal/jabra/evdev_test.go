package jabra

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Event types only the tests use: EV_ABS, and EV_MSC, with which the Jabra
// sends each key's HID usage as an MSC_SCAN event.
const (
	evAbs = 0x03
	evMsc = 0x04
)

func inputEvent(eventType, code uint16, value int32) []byte {
	return stampedEvent(0, eventType, code, value)
}

// stampedEvent is an input_event the kernel stamped at, a clock reading.
func stampedEvent(at time.Duration, eventType, code uint16, value int32) []byte {
	buf := make([]byte, eventSize)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(at/time.Second))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(at%time.Second/time.Microsecond))
	binary.LittleEndian.PutUint16(buf[16:18], eventType)
	binary.LittleEndian.PutUint16(buf[18:20], code)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(value))
	return buf
}

// The office Pi's /proc/bus/input/devices with the Jabra Speak2 75 plugged in
// (2026-11-16), shortened to the blocks this package reads.
const piDevices = `I: Bus=0019 Vendor=0001 Product=0001 Version=0100
N: Name="pwr_button"
H: Handlers=kbd event0 
B: EV=3
B: KEY=10000000000000 0

I: Bus=0003 Vendor=0b0e Product=24ef Version=0111
N: Name="Jabra Speak2 75"
H: Handlers=kbd leds event1 
B: EV=20013
B: KEY=fff 0 0 0 40 100000000000000 0 0 0
B: MSC=10
B: LED=80

I: Bus=0003 Vendor=0b0e Product=24ef Version=0111
N: Name="Jabra Speak2 75"
H: Handlers=event2 
B: EV=9
B: ABS=10000000000

I: Bus=0003 Vendor=0b0e Product=24ef Version=0111
N: Name="Jabra Speak2 75 Consumer Control"
H: Handlers=kbd event3 
B: EV=13
B: KEY=18000 17800000000 8e000000000000 0
B: MSC=10
`

func writeDevices(t *testing.T, devices string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices")
	if err := os.WriteFile(path, []byte(devices), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFindDeviceByNameAndCapability fails if the lookup picks a Jabra node
// without the mute key and light (event2, the consumer-control node), or a
// non-Jabra node that has both.
func TestFindDeviceByNameAndCapability(t *testing.T) {
	path, err := FindDevice(writeDevices(t, piDevices), "/dev/input")
	if err != nil {
		t.Fatalf("find the Jabra mute button: %v", err)
	}
	if path != "/dev/input/event1" {
		t.Fatalf("Jabra path = %q, want /dev/input/event1", path)
	}

	for name, devices := range map[string]string{
		"no Jabra": "N: Name=\"pwr_button\"\nH: Handlers=kbd event0\nB: KEY=10000000000000 0\n",
		"another make with the mute key and light": "N: Name=\"Acme Speakerphone\"\nH: Handlers=kbd leds event5\n" +
			"B: KEY=40 100000000000000 0 0 0\nB: LED=80\n",
		"Jabra without the mute light": "N: Name=\"Jabra Speak2 75\"\nH: Handlers=kbd event1\n" +
			"B: KEY=fff 0 0 0 40 100000000000000 0 0 0\n",
		"Jabra without the mute key": "N: Name=\"Jabra Speak2 75 Consumer Control\"\nH: Handlers=kbd leds event3\n" +
			"B: KEY=18000 17800000000 8e000000000000 0\nB: LED=80\n",
		"Jabra with other lights but not the mute light": "N: Name=\"Jabra Speak2 75\"\nH: Handlers=kbd leds event1\n" +
			"B: KEY=fff 0 0 0 40 100000000000000 0 0 0\nB: LED=7\n",
		"Jabra whose key bitmap is too short to hold the mute key": "N: Name=\"Jabra Speak2 75\"\nH: Handlers=kbd leds event1\n" +
			"B: KEY=40 0\nB: LED=80\n",
	} {
		if _, err := FindDevice(writeDevices(t, devices), "/dev/input"); !errors.Is(err, ErrNoMuteButton) {
			t.Errorf("%s: error = %v, want ErrNoMuteButton", name, err)
		}
	}

	if _, err := FindDevice(filepath.Join(t.TempDir(), "missing"), "/dev/input"); err == nil || errors.Is(err, ErrNoMuteButton) {
		t.Fatalf("missing devices file: error = %v, want a read error", err)
	}
}

// TestReadPressesSendsOneTogglePerPress fails if a release, a repeat, another
// key, another event type with the mute key's code, or the light's own echo
// counts as a press, or if a press does not carry its kernel time stamp, which
// tells the Jabra's own report from a person's tap.
func TestReadPressesSendsOneTogglePerPress(t *testing.T) {
	first := 10296*time.Second + 671885*time.Microsecond
	second := first + 1500*time.Millisecond + 12*time.Microsecond
	var stream bytes.Buffer
	for _, event := range [][]byte{
		inputEvent(evMsc, 4, 0xb002f),
		stampedEvent(first, evKey, keyMicMute, 1),
		inputEvent(evSyn, 0, 0),
		inputEvent(evKey, keyMicMute, 0),
		inputEvent(evSyn, 0, 0),
		inputEvent(evKey, keyMicMute, 2),
		inputEvent(evKey, 115, 1),        // KEY_VOLUMEUP
		inputEvent(evAbs, keyMicMute, 1), // an EV_ABS event with KEY_MICMUTE's code
		inputEvent(evLed, ledMute, 1),
		stampedEvent(second, evKey, keyMicMute, 1),
		inputEvent(evKey, keyMicMute, 0),
	} {
		stream.Write(event)
	}
	presses := make(chan time.Duration, 8)
	if err := ReadPresses(context.Background(), &stream, presses); err != nil {
		t.Fatalf("read presses: %v", err)
	}
	close(presses)
	var got []time.Duration
	for at := range presses {
		got = append(got, at)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("presses = %v, want the two presses stamped %v and %v", got, first, second)
	}
}

func TestReadPressesReportsATruncatedEvent(t *testing.T) {
	stream := bytes.NewReader(inputEvent(evKey, keyMicMute, 1)[:10])
	if err := ReadPresses(context.Background(), stream, make(chan time.Duration, 1)); err == nil {
		t.Fatal("truncated event read without an error")
	}
}

// TestStampWithMonotonicClockRefusesANonEvdevFile fails if the clock request
// on a file that is not an evdev node reports success: the button would then
// compare wall-clock stamps with the monotonic clock.
func TestStampWithMonotonicClockRefusesANonEvdevFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "event1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := StampWithMonotonicClock(f); err == nil {
		t.Fatal("a regular file took the evdev clock request")
	}
}

// stallingReader hands over one event, says so, and then blocks until the
// test ends, as an evdev node with no further events does.
type stallingReader struct {
	event  []byte
	handed chan struct{}
	stall  chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.event == nil {
		<-r.stall
		return 0, io.EOF
	}
	n := copy(p, r.event)
	r.event = r.event[n:]
	if len(r.event) == 0 {
		r.event = nil
		close(r.handed)
	}
	return n, nil
}

// TestReadPressesStopsWithTheContext fails if a press nobody takes keeps
// ReadPresses from returning after its context ends.
func TestReadPressesStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &stallingReader{event: inputEvent(evKey, keyMicMute, 1), handed: make(chan struct{}), stall: make(chan struct{})}
	t.Cleanup(func() { close(reader.stall) })
	done := make(chan error, 1)
	go func() { done <- ReadPresses(ctx, reader, make(chan time.Duration)) }()
	// The press blocks on the unread channel until the context ends.
	<-reader.handed
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read presses after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadPresses did not return after its context ended")
	}
}

type ledEvent struct {
	eventType, code uint16
	value           int32
}

func decodeEvents(t *testing.T, raw []byte) []ledEvent {
	t.Helper()
	if len(raw)%eventSize != 0 {
		t.Fatalf("wrote %d bytes, not whole %d-byte events", len(raw), eventSize)
	}
	var events []ledEvent
	for len(raw) > 0 {
		events = append(events, ledEvent{
			eventType: binary.LittleEndian.Uint16(raw[16:18]),
			code:      binary.LittleEndian.Uint16(raw[18:20]),
			value:     int32(binary.LittleEndian.Uint32(raw[20:24])),
		})
		raw = raw[eventSize:]
	}
	return events
}

// countingWriter records each Write call, because the kernel applies one
// write's events in order and a split write could leave the light half set.
type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestShowMuteLightWritesTheLightThenASync(t *testing.T) {
	for _, on := range []bool{true, false} {
		var w countingWriter
		if err := ShowMuteLight(&w, on); err != nil {
			t.Fatalf("show mute light %t: %v", on, err)
		}
		want := []ledEvent{{evLed, ledMute, boolValue(on)}, {evSyn, 0, 0}}
		if got := decodeEvents(t, w.Bytes()); !slices.Equal(got, want) || w.writes != 1 {
			t.Fatalf("show mute light %t wrote %v in %d writes, want %v in 1", on, got, w.writes, want)
		}
	}
}

// TestResendMuteLightEndsOnTheWantedState fails if a resend can leave the
// light in the other state, or does not pass through it: the kernel sends the
// device a report only when its own copy of the light changes.
func TestResendMuteLightEndsOnTheWantedState(t *testing.T) {
	for _, on := range []bool{true, false} {
		var w countingWriter
		if err := ResendMuteLight(&w, on); err != nil {
			t.Fatalf("resend mute light %t: %v", on, err)
		}
		want := []ledEvent{
			{evLed, ledMute, boolValue(!on)}, {evSyn, 0, 0},
			{evLed, ledMute, boolValue(on)}, {evSyn, 0, 0},
		}
		if got := decodeEvents(t, w.Bytes()); !slices.Equal(got, want) || w.writes != 1 {
			t.Fatalf("resend mute light %t wrote %v in %d writes, want %v in 1", on, got, w.writes, want)
		}
	}
}
