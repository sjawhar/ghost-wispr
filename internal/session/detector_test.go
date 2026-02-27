package session

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestDetectorSilenceTriggersSessionEnd(t *testing.T) {
	detector := NewDetector(30 * time.Millisecond)

	done := make(chan struct{}, 1)
	detector.OnSessionEnd(func() {
		done <- struct{}{}
	})

	detector.OnUtteranceEnd()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected session end callback to fire")
	}
}

func TestDetectorSpeechResetsTimer(t *testing.T) {
	detector := NewDetector(80 * time.Millisecond)

	var fired atomic.Int32
	detector.OnSessionEnd(func() {
		fired.Add(1)
	})

	detector.OnUtteranceEnd()
	time.Sleep(20 * time.Millisecond)
	detector.OnSpeech()

	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("expected 0 callbacks after speech reset, got %d", fired.Load())
	}
}

func TestDetectorSupportsConfigurableTimeout(t *testing.T) {
	short := NewDetector(10 * time.Millisecond)
	long := NewDetector(80 * time.Millisecond)

	shortDone := make(chan struct{}, 1)
	longDone := make(chan struct{}, 1)

	short.OnSessionEnd(func() { shortDone <- struct{}{} })
	long.OnSessionEnd(func() { longDone <- struct{}{} })

	short.OnUtteranceEnd()
	long.OnUtteranceEnd()

	select {
	case <-shortDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected short detector callback")
	}

	select {
	case <-longDone:
		t.Fatal("long timeout should not fire yet")
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case <-longDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected long detector callback")
	}
}
