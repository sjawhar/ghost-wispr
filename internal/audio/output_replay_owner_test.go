package audio

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// discardFirstStream reports, with discard, that the device discarded its
// first frame, as a suspend recovery does, then takes the replay of it.
type discardFirstStream struct {
	discard bool
	writes  atomic.Int32
}

func (s *discardFirstStream) Write([]int16) error { s.writes.Add(1); return nil }

func (s *discardFirstStream) takeAgain() int {
	if s.discard && s.writes.Load() == 1 {
		return 480
	}
	return 0
}

func (*discardFirstStream) Drop() (int, error) { return 0, nil }
func (*discardFirstStream) Close() error       { return nil }

// TestAReplayedSpeechKeepsItsPlaybacksOwner fails if a speech whose audio the
// stream wrote again (#148's replay) loses the owner of the playback it came
// from (SpeechEvent.Owner), on its start or its end: when the device
// discarded the speech's first frame and the mixer wrote it again, and when a
// Cancel of another input took the device's audio back and the mixer wrote
// the voice's part of it again. The control's device discards nothing.
func TestAReplayedSpeechKeepsItsPlaybacksOwner(t *testing.T) {
	type owner struct{ name string }
	for _, discard := range []bool{false, true} {
		name := "control: nothing discarded"
		if discard {
			name = "the device discards the first frame, which is written again"
		}
		t.Run(name, func(t *testing.T) {
			stream := &discardFirstStream{discard: discard}
			out, start, events := probeOutput(t, stream)
			first := &owner{"first"}
			if err := out.Begin(InputVoice, first).Write(testContext(t), constant(48000, 10*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			start()
			began := probeVoiceEvent(t, events, true)
			ended := probeVoiceEvent(t, events, false)
			if n := stream.writes.Load(); discard && n < 2 {
				t.Fatalf("fixture: %d writes, want the discarded frame written again", n)
			}
			if began.Owner != first || ended.Owner != first {
				t.Fatalf("the speech's start carries %v and its end %v, want the playback's owner %v", began.Owner, ended.Owner, first)
			}
		})
	}
	t.Run("a Cancel of another input replays the voice's audio", func(t *testing.T) {
		const rate = 48000
		stream := &replayStream{fakeStream: newFakeStream(rate), back: rate / 20, release: make(chan struct{})}
		close(stream.release)
		out := NewOutput(OutputConfig{Open: func(string) (PlaybackStream, int, error) { return stream, rate, nil }})
		t.Cleanup(func() { _ = out.Close() })
		events := make(chan SpeechEvent, 16)
		defer out.OnSpeech(func(e SpeechEvent) {
			if e.Input == InputVoice {
				events <- e
			}
		})()
		chime := out.Begin(InputChime, nil)
		go func() { _ = chime.WriteAll(testContext(t), constant(rate, time.Second, 7), mono48k) }()
		stream.waitWriting(t)
		first := &owner{"voice"}
		voice := out.Begin(InputVoice, first)
		go func() { _ = voice.WriteAll(testContext(t), constant(rate, 20*time.Millisecond, 1000), mono48k) }()
		// Once the stream has the chime alone after the voice's audio, the
		// voice's audio is in the device's last 50 ms: cancel the chime, so
		// the mixer takes it back and writes the voice's part again.
		for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(time.Millisecond) {
			stream.fakeStream.mu.Lock()
			voiced, after := false, false
			for _, f := range stream.frames {
				switch loudest := slices.Max(f.samples); {
				case loudest > 100:
					voiced = true
				case voiced && loudest > 0:
					after = true
				}
			}
			stream.fakeStream.mu.Unlock()
			if after {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("fixture: the stream never had the chime alone after the voice")
			}
		}
		out.Cancel(InputChime)
		var got []SpeechEvent
		for len(got) < 2 {
			select {
			case e := <-events:
				got = append(got, e)
			case <-time.After(3 * time.Second):
				t.Fatalf("fixture: voice events %+v, want a start and an end", got)
			}
		}
		stream.mu.Lock()
		replayed := stream.replayed
		stream.mu.Unlock()
		if replayed.IsZero() {
			t.Fatal("fixture: the voice's audio was never written again")
		}
		for _, e := range got {
			if e.Owner != first {
				t.Errorf("the voice's %+v carries %v, want the playback's owner %v", e, e.Owner, first)
			}
		}
	})
}
