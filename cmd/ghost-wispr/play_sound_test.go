package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// soundStreamRate is the rate of soundStream's device, the Jabra's: the
// Output resamples the sounds' 24 kHz to it, as on the Pi.
const soundStreamRate = 48000

// soundStream is the speaker's stream in play-sound's tests. Each write takes
// as long as its frame lasts, as a device's does once its buffer is full, so
// the mixer cannot run ahead of the writer: on a stream that never waits it
// may run outputAhead ahead, and a writer the machine held back that long
// leaves a gap inside the sound. It keeps every sample written, the device it
// was opened on, how often it was opened, and whether it closed. With
// failAfterSound, the fifth frame of silence after the sound fails, as a
// device unplugged during the Output's idle frames would: by then Drain has
// returned. With closeErr, Close fails with it and leaves the stream open.
type soundStream struct {
	failAfterSound bool
	closeErr       error

	mu      sync.Mutex
	device  string
	opens   int
	samples []int16
	closed  bool
	heard   bool
	quiet   int // frames of silence written since the sound
}

func (s *soundStream) open(device string) (audio.PlaybackStream, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.device = device
	s.opens++
	return s, soundStreamRate, nil
}

func (s *soundStream) Write(frame []int16) error {
	time.Sleep(time.Duration(len(frame)) * time.Second / soundStreamRate)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, frame...)
	silent := true
	for _, v := range frame {
		silent = silent && v == 0
	}
	if !silent {
		s.heard, s.quiet = true, 0
	} else if s.heard {
		s.quiet++
	}
	if s.failAfterSound && s.quiet == 5 {
		return errors.New("the device went away")
	}
	return nil
}

func (s *soundStream) Drop() (int, error) { return 0, nil }

func (s *soundStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeErr != nil {
		return s.closeErr
	}
	s.closed = true
	return nil
}

// streamState is a soundStream as it was at one moment.
type streamState struct {
	device  string
	opens   int
	samples []int16
	closed  bool
}

func (s *soundStream) state() streamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return streamState{device: s.device, opens: s.opens, samples: append([]int16(nil), s.samples...), closed: s.closed}
}

// playSoundUnit writes a unit's ghost-wispr.yaml holding yaml, with no
// speaker setting from the environment, and returns its path.
func playSoundUnit(t *testing.T, yaml string) string {
	t.Helper()
	t.Setenv("GHOST_WISPR_SPEAKER_ENABLED", "")
	t.Setenv("GHOST_WISPR_SPEAKER_DEVICE", "")
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runPlaySound runs play-sound with args on stream, and returns what it
// printed and the stream as it was when the command returned.
func runPlaySound(t *testing.T, configPath string, stream *soundStream, args ...string) (cliRun, streamState) {
	t.Helper()
	var stdout, stderr cliOutput
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	defer cancel()
	code := runPlaySoundCommand(ctx, append([]string{"play-sound"}, args...), configPath, stream.open, &stdout, &stderr)
	at := stream.state()
	return cliRun{stdout: stdout.String(), stderr: stderr.String(), code: code}, at
}

// TestPlaySoundPlaysTheWholeSoundAndWaitsForTheStreamToClose fails if
// play-sound does not write each sound whole to the stream on the device
// speaker_device names, or returns before the Output has closed that stream
// by itself, IdleClose after the sound: Drain returns once the stream has
// taken the last frame, and a Close then drops what the device still holds,
// the end of the sound the rig microphone must hear. A command that closed
// the Output after Drain would leave about a frame of silence after the
// sound, not half of IdleClose. It also fails if the line it prints does not
// name the sound, its length and the device.
func TestPlaySoundPlaysTheWholeSoundAndWaitsForTheStreamToClose(t *testing.T) {
	configPath := playSoundUnit(t, "speaker_enabled: true\nspeaker_device: J75\n")
	sounds := map[string][]byte{"wake": voice.WakeChime(), "answer": voice.AnswerChime(), "start": voice.StartChime(), "error": voice.ErrorChime()}
	for name, pcm := range sounds {
		t.Run(name, func(t *testing.T) {
			stream := &soundStream{}
			run, at := runPlaySound(t, configPath, stream, name)
			if run.code != 0 {
				t.Fatalf("exit %d, stderr %q; want 0", run.code, run.stderr)
			}
			if !at.closed {
				t.Fatal("play-sound returned before the speaker's stream closed")
			}
			if at.opens != 1 || at.device != "J75" {
				t.Fatalf("the stream opened %d times, on %q; want once, on J75", at.opens, at.device)
			}
			first, last := -1, -1
			for i, s := range at.samples {
				if s != 0 {
					if first < 0 {
						first = i
					}
					last = i
				}
			}
			if first < 0 {
				t.Fatalf("%d samples written, all silent; want the %s sound", len(at.samples), name)
			}
			frames := len(pcm) / (2 * voice.ChimeFormat.Channels)
			want := frames * soundStreamRate / voice.ChimeFormat.SampleRate
			tolerance := soundStreamRate * 5 / 1000 // 5 ms: the resampler's filter at each end
			if span := last - first + 1; span < want-tolerance || span > want+tolerance {
				t.Errorf("the sound spans %d samples at %d Hz, want %d (the %s sound's %d at %d Hz), give or take %d", span, soundStreamRate, want, name, frames, voice.ChimeFormat.SampleRate, tolerance)
			}
			after := time.Duration(len(at.samples)-1-last) * time.Second / soundStreamRate
			if after < audio.OutputIdleClose/2 {
				t.Errorf("the stream closed %v after the sound, want at least %v: the Output's own close comes IdleClose (%v) after it", after, audio.OutputIdleClose/2, audio.OutputIdleClose)
			}
			lines := strings.Split(strings.TrimSuffix(run.stdout, "\n"), "\n")
			var printed playedSound
			if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &printed) != nil {
				t.Fatalf("stdout %q, want one JSON line", run.stdout)
			}
			wantMS := int64(frames * 1000 / voice.ChimeFormat.SampleRate)
			if printed != (playedSound{Sound: name, MS: wantMS, Device: "J75"}) {
				t.Errorf("printed %+v, want sound %q, ms %d, device J75", printed, name, wantMS)
			}
		})
	}
}

// TestPlaySoundRefusesWithoutASoundOrASpeaker fails if play-sound opens the
// speaker for a missing, unknown or extra argument, or does not answer it
// with its usage and exit 2; or if it plays with speaker_enabled false, or
// exits other than 1 with why.
func TestPlaySoundRefusesWithoutASoundOrASpeaker(t *testing.T) {
	configPath := playSoundUnit(t, "speaker_enabled: true\nspeaker_device: J75\n")
	for _, args := range [][]string{{}, {"chime"}, {"Wake"}, {"wake", "answer"}} {
		t.Run(strings.Join(append([]string{"args"}, args...), "-"), func(t *testing.T) {
			stream := &soundStream{}
			run, at := runPlaySound(t, configPath, stream, args...)
			if run.code != 2 || !strings.Contains(run.stderr, playSoundUsage) || run.stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want 2 and %q", run.code, run.stdout, run.stderr, playSoundUsage)
			}
			if at.opens != 0 {
				t.Fatalf("the speaker's stream opened %d times; want none", at.opens)
			}
		})
	}
	t.Run("speaker-disabled", func(t *testing.T) {
		stream := &soundStream{}
		run, at := runPlaySound(t, playSoundUnit(t, "speaker_enabled: false\nspeaker_device: J75\n"), stream, "wake")
		if run.code != 1 || !strings.Contains(run.stderr, "speaker_enabled is false") || run.stdout != "" {
			t.Fatalf("exit %d, stdout %q, stderr %q; want 1 and that speaker_enabled is false", run.code, run.stdout, run.stderr)
		}
		if at.opens != 0 {
			t.Fatalf("the speaker's stream opened %d times; want none", at.opens)
		}
	})
}

// TestPlaySoundFailsWhenTheStreamFailsAfterTheSound fails if play-sound
// reports a sound played when the speaker's stream failed in the silence
// after it: the Output closes its stream then too, but that close is a
// failure, not the idle close play-sound waits for.
func TestPlaySoundFailsWhenTheStreamFailsAfterTheSound(t *testing.T) {
	stream := &soundStream{failAfterSound: true}
	run, _ := runPlaySound(t, playSoundUnit(t, "speaker_enabled: true\nspeaker_device: J75\n"), stream, "wake")
	if run.code != 1 || !strings.Contains(run.stderr, "the device went away") || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 and the stream's error", run.code, run.stdout, run.stderr)
	}
}

// TestPlaySoundFailsWhenTheStreamFailsToClose fails if play-sound reports a
// sound played when the speaker's stream, after the sound, did not close: the
// rig would take a speaker left in a broken state for a good calibration.
func TestPlaySoundFailsWhenTheStreamFailsToClose(t *testing.T) {
	stream := &soundStream{closeErr: errors.New("speaker close failed")}
	run, _ := runPlaySound(t, playSoundUnit(t, "speaker_enabled: true\nspeaker_device: J75\n"), stream, "wake")
	if run.code != 1 || !strings.Contains(run.stderr, "speaker close failed") || run.stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 and the close's error", run.code, run.stdout, run.stderr)
	}
}
