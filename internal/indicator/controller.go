package indicator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

const serverDownTimeout = 3 * time.Second

// waitingFlashEvery is how long LED 2 stays green, then off, while something
// waits for a person; a test shortens it.
var waitingFlashEvery = 500 * time.Millisecond

// The colours of LED 2: red while the unit records, as LED 1; green, then
// off, while something waits for a person.
var (
	led2Red   = [3]uint8{255, 0, 0}
	led2Green = [3]uint8{0, 255, 0}
	led2Off   = [3]uint8{}
)

var ErrNoBlink1 = errors.New("blink(1) indicator not found")

// ErrNotRed reports a light that does not read back red after a red write.
var ErrNotRed = errors.New("recording light does not read back red")

// ErrBusy reports a blink(1) command that could not get the light: another
// open of its node, in this process or another, held the node's lock past the
// wait. A command never gets it for its own handle's other commands, which it
// waits for. The command sent nothing. A read that returns it has no colour:
// the light's colour is unknown, which counts as not red.
var ErrBusy = errors.New("blink(1) busy with another command")

// ErrOtherReply reports a query whose reply belongs to another command: byte
// 1 of a blink(1)'s reply names the command it answers, and another program's
// command reached the device between the query's SET and its GET. The reply
// says nothing about this query, so a read that returns it has no colour,
// which counts as not red.
var ErrOtherReply = errors.New("blink(1) reply is another command's")

// Contended reports an error that says another command had the blink(1), not
// that the light failed: ErrBusy or ErrOtherReply. A read that returns it has
// no colour, which counts as not red.
func Contended(err error) bool {
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrOtherReply)
}

// KnownBlack reports whether light can tell, without sending a command, that
// its unit's stored pattern is black: a check in this process found that
// unit, by its serial number, saved black (a save of the black pattern
// reached it), and the unit was opened again, as after a device error. Its
// watchdog may then be armed at once. A light that cannot tell needs
// BlackPattern.
func KnownBlack(light Light) bool {
	known, ok := light.(interface{ PatternKnownBlack() bool })
	return ok && known.PatternKnownBlack()
}

// PatternCheck is a light's black pattern check run one command per Step, as
// BlackPattern runs it at once, so a caller can send its other commands to
// the light between two steps.
type PatternCheck interface {
	// Step sends the check's next command. done reports that the light's
	// stored pattern is black: it was, or it is now written so.
	Step() (done bool, err error)
}

// NewPatternCheck returns light's stepwise black pattern check. A light that
// has none gets a check whose one step is BlackPattern.
func NewPatternCheck(light Light) PatternCheck {
	if stepwise, ok := light.(interface{ PatternCheck() PatternCheck }); ok {
		return stepwise.PatternCheck()
	}
	return wholePatternCheck{light}
}

// wholePatternCheck is the one-step check of a light with no stepwise one.
type wholePatternCheck struct{ light Light }

func (c wholePatternCheck) Step() (bool, error) {
	if err := c.light.BlackPattern(); err != nil {
		return false, err
	}
	return true, nil
}

// Blink1Report builds an eight-byte blink(1) feature report.
func Blink1Report(command byte, args [6]byte) [8]byte {
	return [8]byte{0x01, command, args[0], args[1], args[2], args[3], args[4], args[5]}
}

// Light controls the recording indicator. SetRGB writes every LED, and
// ReadRGB reads LED 1 back: LED 1 red means the unit records.
type Light interface {
	SetRGB(r, g, b uint8) error
	ReadRGB() (r, g, b uint8, err error)
	ServerDown(timeout time.Duration) error
	BlackPattern() error
	Close() error
}

// setLED2 writes light's LED 2, which shows that something waits for a
// person, and leaves LED 1 as it is. A light with no second LED shows nothing.
func setLED2(light Light, colour [3]uint8) error {
	second, ok := light.(interface{ SetLED2(r, g, b uint8) error })
	if !ok {
		return nil
	}
	return second.SetLED2(colour[0], colour[1], colour[2])
}

// Controller keeps a light red only while its recording signal is true, and
// flashes its LED 2 green while it is red and something waits for a person.
type Controller struct {
	light     Light
	recording func() bool
	interval  time.Duration
	checked   func(error)
	wake      chan struct{}
	waiting   atomic.Bool // something waits for a person (SetWaiting)

	// The flash, Run's goroutine only. flash ticks while LED 2 flashes, nil
	// otherwise; flashGreen says it shows green now, not off. Every write of
	// every LED, whoever sends it, leaves LED 2 showing what LED 1 shows; only
	// the flash makes them differ. led2Dirty says the controller wrote LED 2
	// since its last write of every LED, and led2 is what it wrote.
	flash      *time.Ticker
	flashGreen bool
	led2       [3]uint8
	led2Dirty  bool
}

// NewController returns a controller for light. checked, when not nil, gets
// each check's result.
func NewController(light Light, recording func() bool, interval time.Duration, checked func(error)) *Controller {
	if interval <= 0 {
		interval = time.Second
	}
	return &Controller{
		light:     light,
		recording: recording,
		interval:  interval,
		checked:   checked,
		wake:      make(chan struct{}, 1),
	}
}

// Wake asks Run to re-evaluate the indicator without waiting for the next poll.
func (c *Controller) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// SetWaiting says whether something waits for a person, as the voice reports
// it (voice.WaitingLight): an agent's answer held for "Wispr, go ahead". While
// it does and LED 1 reads red, LED 2 flashes green; otherwise LED 2 shows what
// LED 1 shows. It never blocks.
func (c *Controller) SetWaiting(waiting bool) {
	c.waiting.Store(waiting)
	c.Wake()
}

// Run manages the light until ctx ends. The device's own watchdog restores its
// persisted black pattern when this process stops tickling it.
func (c *Controller) Run(ctx context.Context) {
	defer func() {
		if c.flash != nil {
			c.flash.Stop()
		}
		if err := c.light.SetRGB(0, 0, 0); err != nil {
			slog.Warn("turn recording light off", "error", err)
		} else if err := c.light.ServerDown(0); err != nil {
			slog.Warn("disable recording light watchdog", "error", err)
		}
		if err := c.light.Close(); err != nil {
			slog.Warn("close recording light", "error", err)
		}
	}()

	if err := c.light.BlackPattern(); err != nil && !errors.Is(err, ErrNoBlink1) {
		slog.Warn("save black recording light pattern", "error", err)
	}
	c.update()

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		var flash <-chan time.Time
		if c.flash != nil {
			flash = c.flash.C
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			c.update()
		case <-ticker.C:
			c.update()
		case <-flash:
			c.flashStep()
		}
	}
}

// update tickles the watchdog, sets the colour the recording signal wants,
// shows whether something waits and reports the check to checked.
func (c *Controller) update() {
	_ = c.light.ServerDown(serverDownTimeout)

	var wantRed uint8
	if c.recording() {
		wantRed = 255
	}
	err := c.apply(wantRed)
	c.showWaiting(wantRed == 255 && err == nil)
	if c.checked != nil {
		c.checked(err)
	}
}

// showWaiting starts or stops the flash, as something waits or not, and
// writes LED 2. red says that LED 1 reads red while the unit records: without
// it the flash stops and LED 2 is left alone, since the light's own check
// failed or the off write turned every LED off.
func (c *Controller) showWaiting(red bool) {
	switch flashing := red && c.waiting.Load(); {
	case flashing && c.flash == nil:
		c.flash = time.NewTicker(waitingFlashEvery)
		c.flashGreen = true
	case !flashing && c.flash != nil:
		c.flash.Stop()
		c.flash = nil
	}
	if red {
		c.writeLED2()
	}
}

// flashStep turns LED 2 from green to off or back. Once the unit no longer
// records, it updates instead, which turns every LED off and stops the flash.
func (c *Controller) flashStep() {
	if c.flash == nil {
		return
	}
	if !c.recording() {
		c.update()
		return
	}
	c.flashGreen = !c.flashGreen
	c.writeLED2()
}

// writeLED2 writes the colour LED 2 should show, unless the controller knows
// it shows it: red, as every write of every LED leaves it, when not flashing;
// green or off by turns while flashing. A write that fails is not a failure
// of the light, whose check reads LED 1 alone: the next step or update writes
// LED 2 again.
func (c *Controller) writeLED2() {
	want := led2Red
	if c.flash != nil {
		want = led2Off
		if c.flashGreen {
			want = led2Green
		}
	}
	if want == led2Red && !c.led2Dirty || c.led2Dirty && want == c.led2 {
		return
	}
	if err := setLED2(c.light, want); err != nil {
		slog.Debug("write the answer light", "error", err)
		return
	}
	c.led2, c.led2Dirty = want, want != led2Red
}

// apply makes the light show wantRed. It fails when the light cannot be read
// or written, or, while recording, does not read back red after a red write:
// the recorder then may not capture. An off write that does not read back
// off is not a failure, since a red confirmation can hold the light red.
// While nothing records, a light it cannot read is written off anyway, as
// when another open holds the blink(1)'s lock: a pause turns the light off
// without waiting for the read to get through. Each write writes every LED,
// LED 2 too.
func (c *Controller) apply(wantRed uint8) error {
	red, green, blue, err := c.light.ReadRGB()
	if err != nil {
		if wantRed == 0 && c.light.SetRGB(0, 0, 0) == nil {
			c.led2Dirty = false
		}
		return err
	}
	if red == wantRed && green == 0 && blue == 0 {
		return nil
	}
	if err := c.light.SetRGB(wantRed, 0, 0); err != nil {
		return err
	}
	c.led2Dirty = false
	if wantRed == 0 {
		return nil
	}
	red, green, blue, err = c.light.ReadRGB()
	if err != nil {
		return err
	}
	if red != 255 || green != 0 || blue != 0 {
		return fmt.Errorf("%w: it reads %d,%d,%d", ErrNotRed, red, green, blue)
	}
	return nil
}
