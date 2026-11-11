package powerbutton

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func inputEvent(sec, usec int64, eventType, code uint16, value int32) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(sec))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(usec))
	binary.LittleEndian.PutUint16(buf[16:18], eventType)
	binary.LittleEndian.PutUint16(buf[18:20], code)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(value))
	return buf
}

func TestFindDeviceByName(t *testing.T) {
	devices := `I: Bus=0019 Vendor=0000 Product=0001 Version=0100
N: Name="pwr_button"
H: Handlers=kbd event0

I: Bus=0003 Vendor=0001 Product=0001 Version=0111
N: Name="Keyboard"
H: Handlers=sysrq kbd event1
`
	path, err := FindDevice(filepath.Join(t.TempDir(), "devices"), "/dev/input")
	if err == nil || path != "" {
		t.Fatalf("missing devices file = (%q, %v), want error", path, err)
	}
	devicePath := filepath.Join(t.TempDir(), "devices")
	if err := os.WriteFile(devicePath, []byte(devices), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = FindDevice(devicePath, "/dev/input")
	if err != nil {
		t.Fatalf("find pwr_button: %v", err)
	}
	if path != "/dev/input/event0" {
		t.Fatalf("button path = %q, want /dev/input/event0", path)
	}
	if err := os.WriteFile(devicePath, []byte("N: Name=\"Keyboard\"\nH: Handlers=event1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindDevice(devicePath, "/dev/input"); !errors.Is(err, ErrNoPowerButton) {
		t.Fatalf("button not found error = %v, want ErrNoPowerButton", err)
	}
}

func TestReadEventsDecodesKeyPower(t *testing.T) {
	stream := bytes.Join([][]byte{
		inputEvent(10, 0, 0, 0, 0),
		inputEvent(10, 100, 1, 30, 1),
		inputEvent(10, 150, 1, 116, 2),
		inputEvent(10, 200, 1, 116, 1),
		inputEvent(10, 400, 1, 116, 0),
	}, nil)
	out := make(chan Event, 2)
	if err := ReadEvents(context.Background(), bytes.NewReader(stream), out); err != nil {
		t.Fatalf("read events: %v", err)
	}
	for i, want := range []Event{{Down: true, At: time.Unix(10, 200_000).UTC()}, {Down: false, At: time.Unix(10, 400_000).UTC()}} {
		select {
		case got := <-out:
			if got != want {
				t.Fatalf("event %d = %+v, want %+v", i, got, want)
			}
		default:
			t.Fatalf("event %d missing", i)
		}
	}
}

func TestTapsShortPressIsOneTap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := make(chan Event, 6)
	out := make(chan struct{}, 3)
	done := make(chan struct{})
	go func() { Taps(ctx, in, 1500*time.Millisecond, out); close(done) }()
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	in <- Event{Down: true, At: base}
	in <- Event{Down: false, At: base.Add(200 * time.Millisecond)}
	in <- Event{Down: true, At: base.Add(time.Second)}
	in <- Event{Down: false, At: base.Add(3 * time.Second)}
	in <- Event{Down: true, At: base.Add(4 * time.Second)}
	in <- Event{Down: false, At: base.Add(4200 * time.Millisecond)}
	for i := range 2 {
		select {
		case <-out:
		case <-time.After(time.Second):
			t.Fatalf("tap %d missing", i+1)
		}
	}
	select {
	case <-out:
		t.Fatal("long press emitted a tap")
	default:
	}
	cancel()
	<-done
}
