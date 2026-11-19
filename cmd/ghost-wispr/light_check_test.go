package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/indicator"
)

// stepLight is a blink(1) handle that counts the recorder's commands on it
// at once. Its black pattern check is steps commands of each: BlackPattern
// sends them all, as the driver does; PatternCheck one per Step. The command
// numbered blockAt, when not 0, waits for release once started is closed.
type stepLight struct {
	*reconnectLightFake
	steps   int
	each    time.Duration
	blockAt int
	started chan struct{}
	release chan struct{}

	onHandle      atomic.Int32
	overlaps      atomic.Int32 // commands that found another on the handle
	inCheck       atomic.Bool  // a command of the check is on the handle
	closedInCheck atomic.Int32 // Close calls while it was
	closed        atomic.Bool
	afterClose    atomic.Int32 // commands of the check sent after Close
}

func (l *stepLight) on(command func()) {
	if l.onHandle.Add(1) > 1 {
		l.overlaps.Add(1)
	}
	defer l.onHandle.Add(-1)
	command()
}

func (l *stepLight) checkCommand(n int) {
	l.on(func() {
		l.inCheck.Store(true)
		defer l.inCheck.Store(false)
		if l.closed.Load() {
			l.afterClose.Add(1)
		}
		if n == l.blockAt {
			close(l.started)
			<-l.release
		}
		time.Sleep(l.each)
	})
}

func (l *stepLight) BlackPattern() error {
	for n := 1; n <= l.steps; n++ {
		l.checkCommand(n)
	}
	return nil
}

func (l *stepLight) PatternCheck() indicator.PatternCheck { return &stepCheck{light: l} }

type stepCheck struct {
	light *stepLight
	sent  int
}

func (c *stepCheck) Step() (bool, error) {
	c.sent++
	c.light.checkCommand(c.sent)
	return c.sent == c.light.steps, nil
}

func (l *stepLight) ReadRGB() (r, g, b uint8, err error) {
	l.on(func() {
		time.Sleep(time.Millisecond)
		r, g, b, err = l.reconnectLightFake.ReadRGB()
	})
	return r, g, b, err
}

func (l *stepLight) SetRGB(r, g, b uint8) (err error) {
	l.on(func() {
		time.Sleep(time.Millisecond)
		err = l.reconnectLightFake.SetRGB(r, g, b)
	})
	return err
}

func (l *stepLight) Close() error {
	l.closed.Store(true)
	if l.inCheck.Load() {
		l.closedInCheck.Add(1)
	}
	return l.reconnectLightFake.Close()
}

// TestTheRecordersCommandsTakeTurnsWithItsPatternCheck fails if one of the
// recorder's commands reaches the light's handle while a command of its own
// pattern check is on it. The check ran without deviceMu, so the recorder's
// reads met it on the handle, which the driver took for another program
// (ErrBusy), and its off write landed inside the check's query (review
// 5956273705 of #79). The check now sends one command per hold of deviceMu.
func TestTheRecordersCommandsTakeTurnsWithItsPatternCheck(t *testing.T) {
	device := &stepLight{reconnectLightFake: &reconnectLightFake{colour: [3]uint8{255, 0, 0}}, steps: 33, each: 3 * time.Millisecond}
	light := &reconnectingLight{light: device, patternUnchecked: true, warnings: newWarningsRegistry(), retryEvery: time.Hour}
	checked := make(chan error, 1)
	go func() { checked <- light.CheckPattern() }()
	commands := 0
	for done := false; !done; {
		select {
		case err := <-checked:
			if err != nil {
				t.Fatalf("the pattern check: %v", err)
			}
			done = true
		default:
			if _, _, _, err := light.ReadRGB(); err != nil {
				t.Fatalf("a read during the pattern check: %v", err)
			}
			if err := light.SetRGB(0, 0, 0); err != nil {
				t.Fatalf("an off write during the pattern check: %v", err)
			}
			commands += 2
			time.Sleep(2 * time.Millisecond)
		}
	}
	if commands < 10 {
		t.Fatalf("control: %d of the recorder's commands ran during the pattern check, want at least 10", commands)
	}
	if n := device.overlaps.Load(); n > 0 {
		t.Fatalf("%d of the recorder's commands reached the handle while a command of its own pattern check was on it", n)
	}
	light.deviceMu.Lock()
	unchecked := light.patternUnchecked
	light.deviceMu.Unlock()
	if unchecked {
		t.Fatal("control: the light is still unchecked after its check passed")
	}
}

// TestNoCloseLandsInsideAPatternCheck fails if the recorder's handle is
// closed while a command of its pattern check is on it, or if the check sends
// another command once it is closed: the driver takes the handle's descriptor
// once per command, so a close in between leaves the check on a descriptor
// the next open may get again (review 5956273705 of #79). A Close at shutdown,
// or another command's device error (failed), now waits for the check's
// command, and the check stops and fails.
func TestNoCloseLandsInsideAPatternCheck(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(*reconnectingLight, *stepLight)
	}{
		{"Close at shutdown", func(l *reconnectingLight, _ *stepLight) { _ = l.Close() }},
		{"another command's device error", func(l *reconnectingLight, d *stepLight) {
			d.mu.Lock()
			d.readErr = errors.New("blink(1) feature report: input/output error")
			d.mu.Unlock()
			_, _, _, _ = l.ReadRGB()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := &stepLight{
				reconnectLightFake: &reconnectLightFake{colour: [3]uint8{255, 0, 0}},
				steps:              33, each: time.Millisecond, blockAt: 2,
				started: make(chan struct{}), release: make(chan struct{}),
			}
			release := sync.OnceFunc(func() { close(device.release) })
			t.Cleanup(release)
			original := openBlink1
			openBlink1 = func(string, string) (indicator.Light, error) { return nil, indicator.ErrNoBlink1 }
			t.Cleanup(func() { openBlink1 = original })
			light := &reconnectingLight{light: device, patternUnchecked: true, warnings: newWarningsRegistry(), retryEvery: time.Hour}

			checked := make(chan error, 1)
			go func() { checked <- light.CheckPattern() }()
			select {
			case <-device.started:
			case <-time.After(time.Second):
				t.Fatal("control: the check's second command did not reach the light")
			}
			closed := make(chan struct{})
			go func() { tc.close(light, device); close(closed) }()
			select {
			case <-closed:
			case <-time.After(200 * time.Millisecond):
			}
			release()
			err := <-checked
			<-closed
			if n := device.closedInCheck.Load(); n > 0 {
				t.Fatalf("%s closed the recorder's handle while a command of its pattern check was on it", tc.name)
			}
			if !device.closed.Load() {
				t.Fatalf("control: %s did not close the handle", tc.name)
			}
			if n := device.afterClose.Load(); n > 0 {
				t.Fatalf("the pattern check sent %d commands through the handle after %s closed it", n, tc.name)
			}
			if err == nil {
				t.Fatalf("the pattern check of a handle %s closed under it passed, want it to fail", tc.name)
			}
		})
	}
}
