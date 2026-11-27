package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/ghost-wispr/tools/callprobe/analyze"
)

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// ffmpeg runs ffmpeg with args, its messages on stderr.
func ffmpeg(args []string, stdout, stderr io.Writer) error {
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-nostdin", "-loglevel", "error"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// runPlay plays a file to each -to device at once, from one ffmpeg: to
// callprobe-mic, Meet's microphone, by default. "default" is the sound
// server's default sink, the laptop's speaker.
//
// When its input ends, ffmpeg's PulseAudio output flushes what the server
// still buffers and disconnects without draining it (libavdevice/
// pulse_audio_enc.c, pulse_flash_stream and pulse_write_trailer), which cut
// the last 2 s of each file. So each output asks for a buffer of
// playBufferMS and plays the file with playPad of silence after it: only
// silence is lost, and play returns after the file has played.
func runPlay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("callprobe play", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var to list
	fs.Var(&to, "to", "a sink to play to, more than once for several at once (default callprobe-mic)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: callprobe play [-to DEVICE]... FILE")
		return 2
	}
	file := fs.Arg(0)
	if _, err := os.Stat(file); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe play: %v\n", err)
		return 1
	}
	if len(to) == 0 {
		to = list{"callprobe-mic"}
	}
	ff := []string{"-i", file}
	for i, device := range to {
		ff = append(ff, "-map", "0:a:0", "-af", "apad=pad_dur="+playPad, "-f", "pulse", "-buffer_duration", playBufferMS)
		if device != "default" {
			ff = append(ff, "-device", device)
		}
		ff = append(ff, "callprobe-play-"+strconv.Itoa(i+1)) // the stream's name in the sound server
	}
	_, _ = fmt.Fprintf(stdout, "play file=%s to=%s started=%s\n", file, to.String(), now())
	if err := ffmpeg(ff, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe play: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "play ended=%s\n", now())
	return 0
}

// playBufferMS is the server buffer play asks for, in milliseconds, and
// playPad the silence in seconds it plays after the file, longer than that
// buffer.
const (
	playBufferMS = "200"
	playPad      = "1"
)

// recordName is a recording's name, which is its file's name less .wav.
var recordName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// runRecord records each source to its own WAV file in -out, from one
// ffmpeg, mono 16-bit PCM at -rate, for -for.
func runRecord(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("callprobe record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "the directory of the recordings, made if missing")
	length := fs.Duration("for", 0, "how long to record")
	rate := fs.Int("rate", 48000, "the sample rate")
	var from list
	fs.Var(&from, "from", "NAME=DEVICE: record source DEVICE to NAME.wav, replacing the default of that NAME; more than once")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *out == "" || *length <= 0 || *rate <= 0 {
		_, _ = fmt.Fprintln(stderr, "usage: callprobe record -out DIR -for DURATION [-rate HZ] [-from NAME=DEVICE]...")
		return 2
	}
	sources := [][2]string{
		{"loopback", "callprobe-loopback.monitor"}, // what Meet plays
		{"virtual-mic", "callprobe-mic.monitor"},   // what Meet hears from the laptop
		{"room", "default"},                        // the laptop's own microphone
	}
	for _, f := range from {
		name, device, ok := strings.Cut(f, "=")
		if !ok || !recordName.MatchString(name) || device == "" {
			_, _ = fmt.Fprintf(stderr, "callprobe record: -from %q: want NAME=DEVICE, NAME of a-z, 0-9 and -\n", f)
			return 2
		}
		i := slices.IndexFunc(sources, func(s [2]string) bool { return s[0] == name })
		if i < 0 {
			i = len(sources)
			sources = append(sources, [2]string{name, ""})
		}
		sources[i][1] = device
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe record: %v\n", err)
		return 1
	}
	seconds := strconv.FormatFloat(length.Seconds(), 'f', 3, 64)
	var inputs, outputs []string
	for i, s := range sources {
		path := filepath.Join(*out, s[0]+".wav")
		if _, err := os.Lstat(path); err == nil {
			_, _ = fmt.Fprintf(stderr, "callprobe record: %s is there already: give another -out\n", path)
			return 1
		}
		// ffmpeg's default queue of 8 packets per input blocked on a real
		// server; a device whose packets wait can lose samples.
		inputs = append(inputs, "-f", "pulse", "-thread_queue_size", "4096", "-sample_rate", strconv.Itoa(*rate), "-channels", "1", "-i", s[1])
		outputs = append(outputs, "-map", strconv.Itoa(i)+":a", "-t", seconds, "-c:a", "pcm_s16le", path)
		_, _ = fmt.Fprintf(stdout, "record name=%s device=%s file=%s\n", s[0], s[1], path)
	}
	_, _ = fmt.Fprintf(stdout, "record started=%s for=%v\n", now(), *length)
	if err := ffmpeg(append(append([]string{"-n"}, inputs...), outputs...), stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "callprobe record: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "record ended=%s\n", now())
	// A recording is only the samples ffmpeg wrote, which its data chunk
	// holds: the file's size does not say, as ffmpeg writes a LIST chunk
	// before the data, and so a header alone of 78 bytes.
	failed := false
	for _, s := range sources {
		path := filepath.Join(*out, s[0]+".wav")
		if frames, err := analyze.WAVFrames(path); err != nil || frames == 0 {
			_, _ = fmt.Fprintf(stderr, "callprobe record: ffmpeg wrote no audio to %s from %s\n", path, s[1])
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}
