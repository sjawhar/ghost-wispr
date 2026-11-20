package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
)

var (
	led2Green = [3]uint8{0, 255, 0}
	led2Red   = [3]uint8{255, 0, 0}
)

// TestTheWaitingLightFlashesLED2WhileTheUnitRecords fails if the voice's
// waiting light, wired as main.go wires it, does not flash the blink(1)'s
// LED 2 green and off while the unit records; if LED 1 reads back anything
// but red meanwhile, or the consent check stops the audio for the flash; if
// the consent check passes once LED 1 is not red during the flash; or if LED 2
// does not go back to red once nothing waits.
func TestTheWaitingLightFlashesLED2WhileTheUnitRecords(t *testing.T) {
	r := startLightRig(t, false)
	openMicUnderRed(t, r)
	recorded := &lockedBuffer{}
	writer := r.capture.Writer(recorded)
	readsBefore := len(r.device.snapshot())

	waiting := waitingLED{r.hardware}
	waiting.On()
	waitForHardwareWithin(t, testWait, "LED 2 green, then off", func() bool {
		_, writes := r.device.secondLED()
		return len(writes) >= 2 && writes[0] == led2Green && writes[1] == [3]uint8{}
	})
	if _, err := writer.Write([]byte{1, 2, 3, 4}); err != nil || recorded.Len() != 4 {
		t.Fatalf("a write during the flash = %v, %d bytes recorded; want it recorded: the flash stopped the audio", err, recorded.Len())
	}
	for _, op := range r.device.snapshot()[readsBefore:] {
		if !isRedOp(op) {
			t.Fatalf("LED 1 %v during the flash, want only red: %v", op, r.device.snapshot()[readsBefore:])
		}
	}

	waiting.Off()
	waitForHardwareWithin(t, testWait, "LED 2 back to red", func() bool {
		led2, _ := r.device.secondLED()
		return led2 == led2Red
	})

	waiting.On()
	waitForHardwareWithin(t, testWait, "the flash again", func() bool {
		led2, _ := r.device.secondLED()
		return led2 == led2Green
	})
	r.device.mu.Lock()
	r.device.colour = led2Green // LED 1 goes green outside Ghost Wispr
	r.device.mu.Unlock()
	if _, err := writer.Write([]byte{5, 6, 7, 8}); !errors.Is(err, errLightWentOff) {
		t.Fatalf("a write with LED 1 green during the flash = %v, want errLightWentOff", err)
	}
	if got := recorded.Len(); got != 4 {
		t.Fatalf("%d bytes recorded, want 4: audio passed with LED 1 not red", got)
	}
}

// TestTheWaitingLightNeverFlashesWhilePaused fails if the waiting light writes
// the blink(1)'s LED 2 while the unit is paused.
func TestTheWaitingLightNeverFlashesWhilePaused(t *testing.T) {
	r := startLightRig(t, true)
	waitingLED{r.hardware}.On()
	for range 3 {
		r.hardware.Wake()
		time.Sleep(300 * time.Millisecond) // past the flash's 500 ms steps in all
	}
	if _, writes := r.device.secondLED(); len(writes) != 0 {
		t.Fatalf("LED 2 written %v while paused, want nothing", writes)
	}
	r.waitOff(t)
}

// TestTheWaitingLightStopsWhileNoAudioRecords fails if LED 2 flashes green
// while LED 1 stays red but no audio records: after a pause, until the mic
// loop has closed the microphone, and while a red hold keeps LED 1 red for a
// microphone that reopens. LED 2 shows red then.
func TestTheWaitingLightStopsWhileNoAudioRecords(t *testing.T) {
	for name, stop := range map[string]func(t *testing.T, r *lightRig){
		"a pause, before the microphone closes": func(_ *testing.T, r *lightRig) { r.capture.Pause() },
		"a red hold while the microphone reopens": func(t *testing.T, r *lightRig) {
			r.mic.open.Store(false)
			if err := r.hardware.EnsureRed(); err != nil {
				t.Fatalf("EnsureRed: %v", err)
			}
			t.Cleanup(r.hardware.ReleaseRedHold)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := startLightRig(t, false)
			openMicUnderRed(t, r)
			waitingLED{r.hardware}.On()
			waitForHardwareWithin(t, testWait, "the flash", func() bool {
				led2, _ := r.device.secondLED()
				return led2 == led2Green
			})

			stop(t, r)
			r.hardware.Wake()
			waitForHardwareWithin(t, testWait, "LED 2 red with LED 1 red", func() bool {
				r.device.mu.Lock()
				defer r.device.mu.Unlock()
				return r.device.colour == led2Red && r.device.led2 == led2Red
			})
			_, stopped := r.device.secondLED()
			time.Sleep(1200 * time.Millisecond) // past two of the flash's 500 ms steps
			_, after := r.device.secondLED()
			for _, colour := range after[len(stopped):] {
				if colour != led2Red {
					t.Fatalf("LED 2 written %v while no audio recorded, want red only", after[len(stopped):])
				}
			}
		})
	}
}

// led2Light is a two-LED light whose LED 2 writes fail with led2Err.
type led2Light struct {
	opsLight
	mu2     sync.Mutex
	led2Err error
	tries   int
}

func (l *led2Light) SetLED2(r, g, b uint8) error {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	l.tries++
	if l.led2Err != nil {
		return l.led2Err
	}
	return l.opsLight.SetLED2(r, g, b)
}

// TestALED2WriteIsNotALightFault fails if a write of LED 2 that fails, for
// another program holding the blink(1) or a device error, counts as the
// recording light not red (which stops the audio) or closes the light; or if
// LED 2 is written while the light need not be red.
func TestALED2WriteIsNotALightFault(t *testing.T) {
	for _, failure := range []error{indicator.ErrBusy, errors.New("blink(1) feature report: input/output error")} {
		t.Run(failure.Error(), func(t *testing.T) {
			device := &led2Light{led2Err: failure}
			var hold bool
			light := &reconnectingLight{warnings: newWarningsRegistry(), holdRed: func() bool { return hold }, light: device}
			hold = true
			if err := light.SetLED2(0, 255, 0); !errors.Is(err, failure) {
				t.Fatalf("SetLED2 = %v, want %v", err, failure)
			}
			if got := light.offs.Load(); got != 0 {
				t.Fatalf("a failed LED 2 write counted %d times the light was not red, want 0", got)
			}
			if light.light == nil {
				t.Fatal("a failed LED 2 write closed the light")
			}

			hold = false
			device.mu2.Lock()
			device.led2Err, device.tries = nil, 0
			device.mu2.Unlock()
			if err := light.SetLED2(0, 255, 0); err != nil {
				t.Fatalf("SetLED2 while the light need not be red = %v", err)
			}
			if device.tries != 0 || len(device.led2Writes) != 0 {
				t.Fatalf("LED 2 written %d times while the light need not be red, want none", device.tries)
			}
		})
	}
}
