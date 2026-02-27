package session

import (
	"sync"
	"time"
)

type Detector struct {
	timeout      time.Duration
	mu           sync.Mutex
	timer        *time.Timer
	onSessionEnd func()
}

func NewDetector(timeout time.Duration) *Detector {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Detector{timeout: timeout}
}

func (d *Detector) OnSessionEnd(callback func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onSessionEnd = callback
}

func (d *Detector) OnSpeech() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func (d *Detector) OnUtteranceEnd() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil {
		d.timer.Stop()
	}

	d.timer = time.AfterFunc(d.timeout, func() {
		d.mu.Lock()
		callback := d.onSessionEnd
		d.timer = nil
		d.mu.Unlock()

		if callback != nil {
			callback()
		}
	})
}
