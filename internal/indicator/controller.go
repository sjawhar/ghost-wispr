package indicator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const serverDownTimeout = 3 * time.Second

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
// its unit's stored pattern is black: this process checked that unit, by its
// serial number, and opened it again, as after a device error. Its watchdog
// may then be armed at once. A light that cannot tell needs BlackPattern.
func KnownBlack(light Light) bool {
	known, ok := light.(interface{ PatternKnownBlack() bool })
	return ok && known.PatternKnownBlack()
}

// Blink1Report builds an eight-byte blink(1) feature report.
func Blink1Report(command byte, args [6]byte) [8]byte {
	return [8]byte{0x01, command, args[0], args[1], args[2], args[3], args[4], args[5]}
}

// Light controls the recording indicator.
type Light interface {
	SetRGB(r, g, b uint8) error
	ReadRGB() (r, g, b uint8, err error)
	ServerDown(timeout time.Duration) error
	BlackPattern() error
	Close() error
}

// Controller keeps a light red only while its recording signal is true.
type Controller struct {
	light     Light
	recording func() bool
	interval  time.Duration
	checked   func(error)
	wake      chan struct{}
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

// Run manages the light until ctx ends. The device's own watchdog restores its
// persisted black pattern when this process stops tickling it.
func (c *Controller) Run(ctx context.Context) {
	defer func() {
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
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			c.update()
		case <-ticker.C:
			c.update()
		}
	}
}

// update tickles the watchdog, sets the colour the recording signal wants and
// reports the check to checked.
func (c *Controller) update() {
	_ = c.light.ServerDown(serverDownTimeout)

	var wantRed uint8
	if c.recording() {
		wantRed = 255
	}
	err := c.apply(wantRed)
	if c.checked != nil {
		c.checked(err)
	}
}

// apply makes the light show wantRed. It fails when the light cannot be read
// or written, or, while recording, does not read back red after a red write:
// the recorder then may not capture. An off write that does not read back
// off is not a failure, since a red confirmation can hold the light red.
// While nothing records, a light it cannot read is written off anyway, as
// when another open holds the blink(1)'s lock: a pause turns the light off
// without waiting for the read to get through.
func (c *Controller) apply(wantRed uint8) error {
	red, green, blue, err := c.light.ReadRGB()
	if err != nil {
		if wantRed == 0 {
			_ = c.light.SetRGB(0, 0, 0)
		}
		return err
	}
	if red == wantRed && green == 0 && blue == 0 {
		return nil
	}
	if err := c.light.SetRGB(wantRed, 0, 0); err != nil {
		return err
	}
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
