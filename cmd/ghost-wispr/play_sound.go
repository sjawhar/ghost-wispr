package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const playSoundUsage = "usage: ghost-wispr play-sound <wake|answer|start|error>"

// playSounds are Wispr's sounds by the names play-sound takes
// (internal/voice/chime.go).
var playSounds = map[string]func() []byte{
	"wake":   voice.WakeChime,
	"answer": voice.AnswerChime,
	"start":  voice.StartChime,
	"error":  voice.ErrorChime,
}

// playSoundCloseWait is how much longer than the sound play-sound waits for
// the Output to close its stream.
const playSoundCloseWait = 5 * time.Second

// playedSound is the line play-sound prints once the sound has played: its
// name, its length in milliseconds, and the speaker_device it played on.
type playedSound struct {
	Sound  string `json:"sound"`
	MS     int64  `json:"ms"`
	Device string `json:"device"`
}

// runPlaySoundCommand is ghost-wispr play-sound NAME: one of Wispr's sounds,
// played once on the speaker device the config names, as the voice plays it:
// on an Output's chime input, in voice.ChimeFormat. It returns once the
// Output has closed its stream by itself, IdleClose after the sound, when the
// device holds only silence: Drain returns when the stream has taken the last
// frame, and Close would drop what the device still holds. Then it prints
// {"sound":NAME,"ms":LENGTH,"device":DEVICE}. accept-voice.sh's
// playback-cancel step plays the wake sound alone with it, to learn how long
// the rig microphone hears that sound.
//
// It runs beside the unit, whose guard of the Jabra's light does not see its
// stream: run it while the unit changes no capture state, as that step does.
// open opens the stream, for tests; main passes nil, the speaker device. A
// wrong argument exits 2; speaker_enabled false, or a failure, 1 with why on
// stderr.
func runPlaySoundCommand(ctx context.Context, args []string, configPath string, open func(device string) (audio.PlaybackStream, int, error), stdout, stderr io.Writer) int {
	var sound func() []byte
	if len(args) == 2 {
		sound = playSounds[args[1]]
	}
	if sound == nil {
		_, _ = fmt.Fprintln(stderr, playSoundUsage)
		return 2
	}
	name := args[1]
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "play-sound %s: %v\n", name, err)
		return 1
	}
	cfgStore, _, err := loadRuntimeConfig(configPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}
	cfg := cfgStore.Get()
	if !cfg.SpeakerEnabled {
		return fail(errors.New("speaker_enabled is false: the unit has no speaker to play it on"))
	}
	pcm := sound()
	frames := len(pcm) / (2 * voice.ChimeFormat.Channels)
	length := time.Duration(frames) * time.Second / time.Duration(voice.ChimeFormat.SampleRate)
	ctx, cancel := context.WithTimeout(ctx, length+playSoundCloseWait)
	defer cancel()
	closed := make(chan struct{})
	var once sync.Once
	out := audio.NewOutput(audio.OutputConfig{
		Device: cfg.SpeakerDevice,
		Closing: func(closeStream func()) {
			closeStream()
			once.Do(func() { close(closed) })
		},
		Open: open,
	})
	defer func() { _ = out.Close() }()
	pb := out.Begin(audio.InputChime)
	if err := pb.WriteAll(ctx, pcm, voice.ChimeFormat); err != nil {
		return fail(err)
	}
	if err := pb.Drain(ctx); err != nil {
		return fail(err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		return fail(fmt.Errorf("the speaker's stream did not close by itself within %v", length+playSoundCloseWait))
	}
	// A stream that failed closes too: then the playback ended with its error.
	if err := pb.Drain(ctx); err != nil {
		return fail(fmt.Errorf("the speaker's stream failed after the sound: %w", err))
	}
	if err := out.Close(); err != nil {
		return fail(err)
	}
	line, err := json.Marshal(playedSound{Sound: name, MS: length.Milliseconds(), Device: cfg.SpeakerDevice})
	if err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", line); err != nil {
		return fail(err)
	}
	return 0
}
