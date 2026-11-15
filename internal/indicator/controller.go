package indicator

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const serverDownTimeout = 3 * time.Second

var ErrNoBlink1 = errors.New("blink(1) indicator not found")

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
	wake      chan struct{}
}

func NewController(light Light, recording func() bool, interval time.Duration) *Controller {
	if interval <= 0 {
		interval = time.Second
	}
	return &Controller{
		light:     light,
		recording: recording,
		interval:  interval,
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

func (c *Controller) update() {
	_ = c.light.ServerDown(serverDownTimeout)

	var wantRed uint8
	if c.recording() {
		wantRed = 255
	}
	red, green, blue, err := c.light.ReadRGB()
	if err != nil {
		return
	}
	if red == wantRed && green == 0 && blue == 0 {
		return
	}
	_ = c.light.SetRGB(wantRed, 0, 0)
}
