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

// usePortAudioPlayback runs the rest of the test with the speaker's playback
// going through PortAudio, as on systems without ALSA.
func usePortAudioPlayback(t *testing.T) {
	original := playbackUsesPortAudio
	playbackUsesPortAudio = true
	t.Cleanup(func() { playbackUsesPortAudio = original })
}

// PortAudio is never torn down under a speaker's PortAudio output stream,
// which crashes inside PortAudio: re-initializing it (a mic reconnect, the mic
// startup retry) or terminating it (shutdown) first stops the playback and
// waits for its stream to close. (On Linux playback goes through ALSA:
// TestPortAudioTeardownLeavesALSAPlaybackPlaying.)
func TestPortAudioTeardownStopsAndWaitsForSpeakerPlayback(t *testing.T) {
	usePortAudioPlayback(t)
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
	usePortAudioPlayback(t)
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

// TestPortAudioTeardownLeavesALSAPlaybackPlaying fails if a re-initialization
// of PortAudio, as a microphone reconnect makes, stops a playback that goes
// through ALSA, which does not depend on PortAudio, or waits for it.
func TestPortAudioTeardownLeavesALSAPlaybackPlaying(t *testing.T) {
	original := playbackUsesPortAudio
	playbackUsesPortAudio = false
	t.Cleanup(func() { playbackUsesPortAudio = original })
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
	origTerminate, origInitialize := paTerminate, paInitialize
	paTerminate = func() error { return nil }
	paInitialize = func() error { return nil }
	t.Cleanup(func() { paTerminate, paInitialize = origTerminate, origInitialize })

	pcm := makePCM16Bytes(make([]int16, 16000*2)) // two seconds of audio: eight writes
	played := make(chan *PlaybackResult, 1)
	go func() {
		result, _ := speaker.Play(pcm, AudioFormat{SampleRate: 16000, Channels: 1, Encoding: EncodingPCM16})
		played <- result
	}()
	<-writing
	reinit := make(chan error, 1)
	go func() { reinit <- ReinitPortAudio() }()
	select {
	case err := <-reinit:
		if err != nil {
			t.Fatalf("ReinitPortAudio = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReinitPortAudio waited for an ALSA playback")
	}
	select {
	case result := <-played:
		if result == nil || result.BytesWritten != int64(len(pcm)) {
			t.Errorf("playback = %+v, want all %d bytes played: a PortAudio re-initialization stopped an ALSA playback", result, len(pcm))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("speaker still playing 10s after the re-initialization")
	}
}
