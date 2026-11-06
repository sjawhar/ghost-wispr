package audio

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gordonklaus/portaudio"
)

func TestNewMicUsesOpenInputStreamFn(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})

	wantErr := errors.New("boom")
	openInputStreamFn = func(sampleRate, framesPerBuffer int, buf []int16) (*portaudio.Stream, error) {
		if sampleRate != 16000 {
			t.Fatalf("unexpected sample rate: %d", sampleRate)
		}
		if framesPerBuffer != 4000 {
			t.Fatalf("unexpected framesPerBuffer: %d", framesPerBuffer)
		}
		if len(buf) != 4000 {
			t.Fatalf("unexpected buffer len: %d", len(buf))
		}
		return nil, wantErr
	}

	_, err := NewMic(16000, 4000)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

func TestMicReopenFailureClearsStreamAndMarksInactive(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})

	openInputStreamFn = func(sampleRate, framesPerBuffer int, buf []int16) (*portaudio.Stream, error) {
		return nil, errors.New("open failed")
	}

	mic := &Mic{
		sampleRate:      16000,
		framesPerBuffer: 4000,
		buf:             make([]int16, 4000),
	}
	mic.active.Store(true)

	err := mic.Reopen()
	if err == nil {
		t.Fatal("expected reopen error")
	}
	if !strings.Contains(err.Error(), "open failed") {
		t.Fatalf("expected open failure in error, got %v", err)
	}
	if mic.stream != nil {
		t.Fatal("expected nil stream after failed reopen")
	}
	if mic.active.Load() {
		t.Fatal("expected inactive mic after failed reopen")
	}
}

func TestMicStreamReturnsErrorWhenStreamUnavailable(t *testing.T) {
	mic := &Mic{}

	err := mic.Stream(bytes.NewBuffer(nil))
	if err == nil {
		t.Fatal("expected stream error")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMicStopIsSafeWhenStreamUnavailable(t *testing.T) {
	mic := &Mic{}
	mic.active.Store(true)

	if err := mic.Stop(); err != nil {
		t.Fatalf("expected nil stop error, got %v", err)
	}
	if mic.active.Load() {
		t.Fatal("expected inactive mic after stop")
	}
}

// IsOpen is read from HTTP handlers (status "capturing", health) while the mic
// loop reopens the stream; under -race this must not be a data race.
func TestMicIsOpenIsSafeDuringReopen(t *testing.T) {
	original := openInputStreamFn
	t.Cleanup(func() {
		openInputStreamFn = original
	})
	// A stream that opens but cannot start makes Reopen set and then clear m.stream.
	openInputStreamFn = func(sampleRate, framesPerBuffer int, buf []int16) (*portaudio.Stream, error) {
		return &portaudio.Stream{}, nil
	}

	mic := &Mic{sampleRate: 16000, framesPerBuffer: 4000, buf: make([]int16, 4000)}
	mic.active.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			_ = mic.IsOpen()
		}
	}()
	for range 5 {
		_ = mic.Reopen()
	}
	<-done

	if mic.IsOpen() {
		t.Fatal("expected a mic whose reopen failed to report closed")
	}
}
