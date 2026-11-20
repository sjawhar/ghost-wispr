package audio

import (
	"errors"
	"testing"
	"time"

	"github.com/gordonklaus/portaudio"
)

// usePortAudioPlayback runs the rest of the test with the speaker's stream
// going through PortAudio, as on systems without ALSA.
func usePortAudioPlayback(t *testing.T) {
	original := playbackUsesPortAudio
	playbackUsesPortAudio = true
	t.Cleanup(func() { playbackUsesPortAudio = original })
}

// TestPortAudioOutputDeviceIsTheConfiguredOne fails if, off Linux, the
// speaker opens the default output device while speaker_device names
// another, or falls back to the default when the named one is missing: the
// speech would come from the laptop's speakers instead of the Jabra. With no
// speaker_device, the default is the device.
func TestPortAudioOutputDeviceIsTheConfiguredOne(t *testing.T) {
	speakers := &portaudio.DeviceInfo{Name: "MacBook Pro Speakers", MaxOutputChannels: 2, DefaultSampleRate: 44100}
	jabraMic := &portaudio.DeviceInfo{Name: "Jabra Speak2 75 (input)", MaxInputChannels: 1, DefaultSampleRate: 32000}
	jabra := &portaudio.DeviceInfo{Name: "Jabra Speak2 75", MaxOutputChannels: 2, DefaultSampleRate: 48000}
	devices := []*portaudio.DeviceInfo{speakers, jabraMic, jabra}
	for _, tc := range []struct {
		name string
		want *portaudio.DeviceInfo
	}{
		{name: "Jabra", want: jabra},
		{name: "jabra speak2", want: jabra},
		{name: "", want: speakers},
	} {
		got, err := portAudioOutputDevice(devices, speakers, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("speaker_device %q opens %v (%v), want %s", tc.name, deviceName(got), err, tc.want.Name)
		}
	}
	if got, err := portAudioOutputDevice(devices, speakers, "Polycom"); err == nil {
		t.Errorf("speaker_device Polycom, which no device matches, opens %s, want an error", deviceName(got))
	}
}

func deviceName(d *portaudio.DeviceInfo) string {
	if d == nil {
		return "nothing"
	}
	return d.Name
}

// TestPortAudioTeardownClosesThePlayingOutput fails if PortAudio is torn down
// under the speaker's PortAudio output stream, which crashes inside
// PortAudio: re-initializing it (a mic reconnect, the mic startup retry) or
// terminating it (shutdown) first stops the output and waits for its stream
// to close. The playback under way ends cancelled, and the next one opens a
// new stream once the teardown is over. (On Linux playback goes through ALSA:
// TestPortAudioTeardownLeavesALSAOutputPlaying.)
func TestPortAudioTeardownClosesThePlayingOutput(t *testing.T) {
	usePortAudioPlayback(t)
	for _, tc := range []struct {
		name     string
		teardown func() error
	}{
		{name: "reinitialize", teardown: ReinitPortAudio},
		{name: "terminate", teardown: TerminatePortAudio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			playing, next := newFakeStream(48000), newFakeStream(48000)
			playing.block = make(chan struct{}) // every write waits, until the teardown has stopped the playback
			out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{playing, next}})
			terminated, streamOpen := false, false
			origTerminate, origInitialize := paTerminate, paInitialize
			paTerminate = func() error {
				terminated, streamOpen = true, !playing.isClosed()
				return nil
			}
			paInitialize = func() error { return nil }
			t.Cleanup(func() { paTerminate, paInitialize = origTerminate, origInitialize })

			ctx := testContext(t)
			pb := out.Begin(InputVoice)
			written := make(chan error, 1)
			go func() { written <- pb.WriteAll(ctx, constant(48000, time.Minute, 1000), mono48k) }()
			playing.waitWriting(t)

			tornDown := make(chan error, 1)
			go func() { tornDown <- tc.teardown() }()
			select {
			case err := <-written:
				if !errors.Is(err, ErrCancelled) {
					t.Fatalf("the playback under the teardown = %v, want ErrCancelled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("speaker still playing 10s after the teardown began")
			}
			// The teardown has stopped the playback while a write of the
			// stream waits. The Output must close its stream before its next
			// frame, not once it has been idle (OutputIdleClose): a mic
			// reconnect waits for the close. Counting the frames the stream
			// is given, not the time the teardown takes, keeps load out of it.
			before := len(playing.played())
			close(playing.block)
			select {
			case err := <-tornDown:
				if err != nil {
					t.Fatalf("teardown: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the teardown did not return in 10s")
			}
			if more := len(playing.played()) - before; more > 0 {
				t.Fatalf("the speaker's stream was given %d more frames after the teardown stopped its playback, want it closed before the next frame", more)
			}
			if !terminated {
				t.Fatal("PortAudio was not terminated")
			}
			if streamOpen {
				t.Fatal("PortAudio terminated while the speaker's output stream was still open")
			}
			pb = out.Begin(InputVoice)
			if err := pb.WriteAll(ctx, constant(48000, 20*time.Millisecond, 1000), mono48k); err != nil {
				t.Fatal(err)
			}
			if err := pb.Drain(ctx); err != nil {
				t.Fatalf("a playback after the teardown: %v", err)
			}
			if silent(next.samples()) {
				t.Fatal("a playback after the teardown did not play on a new stream")
			}
		})
	}
}

// TestPortAudioTeardownLeavesPortAudioUpUnderAStuckOutput fails if a teardown
// waits for good on a speaker stream stuck in a write (a wedged output
// device), or tears PortAudio down under it: it must report the stream still
// open and leave PortAudio as it is, as shutdown does for a mic loop stuck in
// a read.
func TestPortAudioTeardownLeavesPortAudioUpUnderAStuckOutput(t *testing.T) {
	usePortAudioPlayback(t)
	stuck := newFakeStream(48000)
	stuck.block = make(chan struct{})
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stuck}})
	terminated := false
	origTerminate, origInitialize, origWait := paTerminate, paInitialize, outputStopWait
	paTerminate = func() error { terminated = true; return nil }
	paInitialize = func() error { return nil }
	outputStopWait = 50 * time.Millisecond
	t.Cleanup(func() { paTerminate, paInitialize, outputStopWait = origTerminate, origInitialize, origWait })

	ctx := testContext(t)
	go func() { _ = out.Begin(InputVoice).WriteAll(ctx, constant(48000, time.Second, 1000), mono48k) }()
	stuck.waitWriting(t)
	if err := ReinitPortAudio(); err == nil {
		t.Error("ReinitPortAudio succeeded with a speaker stream stuck open")
	}
	if terminated {
		t.Error("PortAudio terminated under a speaker stream stuck open")
	}
	close(stuck.block)
}

// TestPortAudioTeardownLeavesALSAOutputPlaying fails if a re-initialization
// of PortAudio, as a microphone reconnect makes, stops an output that plays
// through ALSA, which does not depend on PortAudio, or waits for it.
func TestPortAudioTeardownLeavesALSAOutputPlaying(t *testing.T) {
	original := playbackUsesPortAudio
	playbackUsesPortAudio = false
	t.Cleanup(func() { playbackUsesPortAudio = original })
	stream := newFakeStream(48000)
	out := newTestOutput(t, OutputConfig{}, &fakeOpener{streams: []*fakeStream{stream}})
	origTerminate, origInitialize := paTerminate, paInitialize
	paTerminate = func() error { return nil }
	paInitialize = func() error { return nil }
	t.Cleanup(func() { paTerminate, paInitialize = origTerminate, origInitialize })

	ctx := testContext(t)
	pb := out.Begin(InputVoice)
	played := make(chan error, 1)
	go func() {
		err := pb.WriteAll(ctx, constant(48000, 300*time.Millisecond, 1000), mono48k)
		if err == nil {
			err = pb.Drain(ctx)
		}
		played <- err
	}()
	stream.waitWriting(t)
	reinit := make(chan error, 1)
	go func() { reinit <- ReinitPortAudio() }()
	select {
	case err := <-reinit:
		if err != nil {
			t.Fatalf("ReinitPortAudio = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReinitPortAudio waited for an ALSA output")
	}
	if err := <-played; err != nil {
		t.Fatalf("the playback = %v, want it all played: a PortAudio re-initialization stopped an ALSA output", err)
	}
	if played := pb.Played(); played != 300*time.Millisecond {
		t.Fatalf("played %v, want all 300 ms", played)
	}
}
