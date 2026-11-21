package gptlive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

func TestSolReviewRoomAudioWhileCloseWaits(t *testing.T) {
	tap := make(chan audio.Frame, 4)
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	x.Tap = tap
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, x)
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	c.expect(evSessionClose)
	tap <- audio.Frame{PCM: ramp(2000, 4000), Rate: 16000, Channels: 1}
	select {
	case ev := <-c.in:
		if ev.Type == evInputAudioAppend {
			t.Error("room audio sent after session.close while Close waits for finalization")
		}
	case <-time.After(30 * time.Millisecond):
	}
	c.closed()
	<-done
	// Control: audio queued after Close returns cannot reach the vendor.
	tap <- audio.Frame{PCM: ramp(9000, 4000), Rate: 16000, Channels: 1}
	c.noAudio(50 * time.Millisecond)
}

func TestSolReviewCancelledExchangeDoesNotFlushHeldFrame(t *testing.T) {
	tap := make(chan audio.Frame, 4)
	x := testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{}))
	x.Tap = tap
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := newLiveServer(t)
	ss, c, _ := openSessionCtx(t, srv, ctx, x)
	s := ss.(*session)
	// Hold the socket writer while the tap pump takes a frame.
	s.writeMu.Lock()
	tap <- audio.Frame{PCM: ramp(3000, 4000), Rate: 16000, Channels: 1}
	until := time.Now().Add(time.Second)
	for len(tap) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond)
	cancel()
	s.writeMu.Unlock()
	audioAfter := 0
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
loop:
	for {
		select {
		case ev := <-c.in:
			if ev.Type == evInputAudioAppend {
				audioAfter++
			}
			if ev.Type == evSessionClose {
				c.closed()
			}
		case <-timer.C:
			break loop
		}
	}
	_ = s.Close()
	if audioAfter > 0 {
		t.Fatalf("%d held room frame sent after exchange cancellation", audioAfter)
	}
}

func TestSolReviewCloseFrameMustNotLogRoomWords(t *testing.T) {
	// The server can give a free-form close reason independently of APIError.Message.
	srv := newLiveServer(t)
	s, c, _ := openSession(t, srv, testExchange(newFakeOutput(), newFakeTools(voice.AnswerResult{})))
	const words = "sol-review-room-words-marker"
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, words), time.Now().Add(time.Second))
	ev := waitEvent(t, s, voice.EventErr)
	if strings.Contains(ev.Err.Error(), words) {
		t.Fatal("room words from WebSocket close text reach EventErr and the journal")
	}
}
