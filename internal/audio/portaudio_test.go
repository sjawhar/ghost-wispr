package audio

import (
	"sync"
	"testing"
	"time"
)

// pacedOutputStream writes like a device: each Write takes a moment.
type pacedOutputStream struct {
	mock    *mockOutputStream
	onWrite func()
}

func (p *pacedOutputStream) Start() error { return p.mock.Start() }
func (p *pacedOutputStream) Stop() error  { return p.mock.Stop() }
func (p *pacedOutputStream) Close() error { return p.mock.Close() }

func (p *pacedOutputStream) Write() error {
	p.onWrite()
	return p.mock.Write()
}

// PortAudio is never torn down under a speaker's output stream, which crashes
// inside PortAudio: re-initializing it (a mic reconnect, the mic startup retry)
// or terminating it (shutdown) first stops the playback and waits for its
// stream to close.
func TestPortAudioTeardownStopsAndWaitsForSpeakerPlayback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		teardown func() error
	}{
		{name: "reinitialize", teardown: ReinitPortAudio},
		{name: "terminate", teardown: TerminatePortAudio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockOutputStream{}
			writing := make(chan struct{})
			var once sync.Once
			speaker := NewSpeaker("")
			speaker.openStream = func(_, _, _ int, buf []int16, _ string) (outputStream, error) {
				mock.buf = buf
				return &pacedOutputStream{mock: mock, onWrite: func() {
					once.Do(func() { close(writing) })
					time.Sleep(time.Millisecond)
				}}, nil
			}

			terminated, streamOpen := false, false
			origTerminate, origInitialize := paTerminate, paInitialize
			paTerminate = func() error {
				mock.mu.Lock()
				defer mock.mu.Unlock()
				terminated, streamOpen = true, !mock.closed
				return nil
			}
			paInitialize = func() error { return nil }
			t.Cleanup(func() { paTerminate, paInitialize = origTerminate, origInitialize })

			pcm := makePCM16Bytes(make([]int16, 16000*60)) // a minute of audio
			played := make(chan *PlaybackResult, 1)
			go func() {
				result, _ := speaker.Play(pcm, AudioFormat{SampleRate: 16000, Channels: 1, Encoding: EncodingPCM16})
				played <- result
			}()
			<-writing

			if err := tc.teardown(); err != nil {
				t.Fatalf("teardown: %v", err)
			}
			if !terminated {
				t.Fatal("PortAudio was not terminated")
			}
			if streamOpen {
				t.Error("PortAudio terminated while the speaker's output stream was still open")
			}
			select {
			case result := <-played:
				if result == nil || result.BytesWritten >= int64(len(pcm)) {
					t.Errorf("playback = %+v, want it stopped early by the teardown", result)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("speaker still playing 10s after the teardown")
			}
		})
	}
}

// A speaker stream stuck in a write (a wedged output device) never lets its
// playback stop. Teardown does not wait for it for good, and does not tear
// PortAudio down under it: it reports the stream still open and leaves
// PortAudio as it is, as shutdown does for a mic loop stuck in a read.
func TestPortAudioTeardownLeavesPortAudioUpUnderAStuckSpeaker(t *testing.T) {
	mock := &mockOutputStream{}
	writing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	speaker := NewSpeaker("")
	speaker.openStream = func(_, _, _ int, buf []int16, _ string) (outputStream, error) {
		mock.buf = buf
		return &pacedOutputStream{mock: mock, onWrite: func() {
			once.Do(func() { close(writing) })
			<-release
		}}, nil
	}
	terminated := false
	origTerminate, origInitialize, origWait := paTerminate, paInitialize, outputStopWait
	paTerminate = func() error { terminated = true; return nil }
	paInitialize = func() error { return nil }
	outputStopWait = 50 * time.Millisecond
	t.Cleanup(func() { paTerminate, paInitialize, outputStopWait = origTerminate, origInitialize, origWait })

	played := make(chan struct{})
	go func() {
		defer close(played)
		_, _ = speaker.Play(makePCM16Bytes(make([]int16, 16000)), AudioFormat{SampleRate: 16000, Channels: 1, Encoding: EncodingPCM16})
	}()
	<-writing

	if err := ReinitPortAudio(); err == nil {
		t.Error("ReinitPortAudio succeeded with a speaker stream stuck open")
	}
	if terminated {
		t.Error("PortAudio terminated under a speaker stream stuck open")
	}
	close(release)
	<-played
}
