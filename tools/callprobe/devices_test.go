package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fakePactl is pactl as setup-linux.sh sees it, kept in a state file of
// loaded modules ("<index>\t<module>\t<arguments>"), with only bash's own
// commands. It logs each call's arguments, one call a line.
const fakePactl = `state=$FAKE_PACTL_STATE
printf '%s\n' "$*" >>"$FAKE_PACTL_LOG"
names() { # names KEY ARGS: the value of each KEY= in ARGS
	local key=$1 kv
	read -ra words <<<"$2"
	for kv in "${words[@]}"; do
		[[ $kv == "$key"=* ]] && printf '%s\n' "${kv#"$key"=}"
	done
}
case "$1 ${2:-} ${3:-}" in
"list short modules")
	while IFS= read -r line; do printf '%s\n' "$line"; done <"$state" ;;
"list short sinks")
	while IFS=$'\t' read -r index module args; do
		[[ $module == module-null-sink ]] || continue
		while read -r name; do printf '%s\t%s\tmodule-null-sink.c\ts16le 1ch 48000Hz\tIDLE\n' "$index" "$name"; done < <(names sink_name "$args")
	done <"$state" ;;
"list short sources")
	while IFS=$'\t' read -r index module args; do
		case $module in
		module-null-sink) while read -r name; do printf '%s\t%s.monitor\tmodule-null-sink.c\ts16le 1ch 48000Hz\tIDLE\n' "$index" "$name"; done < <(names sink_name "$args") ;;
		module-remap-source) while read -r name; do printf '%s\t%s\tmodule-remap-source.c\ts16le 1ch 48000Hz\tIDLE\n' "$index" "$name"; done < <(names source_name "$args") ;;
		esac
	done <"$state" ;;
*)
	case $1 in
	load-module)
		next=0
		while IFS=$'\t' read -r index _; do (( index >= next )) && next=$((index + 1)); done <"$state"
		printf '%s\t%s\t%s\n' "$next" "$2" "${*:3}" >>"$state"
		printf '%s\n' "$next" ;;
	unload-module)
		kept=() found=no
		while IFS= read -r line; do
			if [[ ${line%%$'\t'*} == "$2" ]]; then found=yes; else kept+=("$line"); fi
		done <"$state"
		[[ $found == yes ]] || { echo "Failure: No such entity" >&2; exit 1; }
		printf '%s\n' "${kept[@]}" >"$state" ;;
	*) echo "fake pactl: no such call: $*" >&2; exit 1 ;;
	esac ;;
esac
`

// setupPactl puts the fake pactl on PATH, with one module loaded that is not
// callprobe's, and returns the paths of its state and its log.
func setupPactl(t *testing.T) (state, log string) {
	t.Helper()
	dir := fakePath(t, map[string]string{"pactl": fakePactl})
	state, log = filepath.Join(dir, "state"), filepath.Join(dir, "log")
	if err := os.WriteFile(state, []byte("7\tmodule-null-sink\tsink_name=other-sink\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PACTL_STATE", state)
	t.Setenv("FAKE_PACTL_LOG", log)
	return state, log
}

// calls is the logged pactl calls that start with verb.
func calls(t *testing.T, log, verb string) []string {
	t.Helper()
	var out []string
	for _, l := range lines(t, log) {
		if strings.HasPrefix(l, verb+" ") {
			out = append(out, l)
		}
	}
	return out
}

// moduleArg is the value of key= in a logged load-module call.
func moduleArg(call, key string) string {
	for _, w := range strings.Fields(call) {
		if v, ok := strings.CutPrefix(w, key+"="); ok {
			return v
		}
	}
	return ""
}

// setup makes callprobe-mic, Meet's microphone through the source
// callprobe-mic-in (Chrome lists no monitor as a microphone), and
// callprobe-loopback, Meet's speaker; run again, it makes nothing.
func TestSetupMakesEachDeviceOnce(t *testing.T) {
	state, log := setupPactl(t)
	if out, errOut, code := callprobe(t, "", "setup"); code != 0 {
		t.Fatalf("setup exited %d: %s%s", code, out, errOut)
	}
	loads := calls(t, log, "load-module")
	var sinks []string
	var remaps []string
	for _, call := range loads {
		switch strings.Fields(call)[1] {
		case "module-null-sink":
			sinks = append(sinks, moduleArg(call, "sink_name"))
		case "module-remap-source":
			remaps = append(remaps, moduleArg(call, "master")+" as "+moduleArg(call, "source_name"))
		default:
			t.Errorf("setup loaded %s", call)
		}
	}
	slices.Sort(sinks)
	if want := []string{"callprobe-loopback", "callprobe-mic"}; !slices.Equal(sinks, want) {
		t.Errorf("setup made the null sinks %q, want %q", sinks, want)
	}
	if want := []string{"callprobe-mic.monitor as callprobe-mic-in"}; !slices.Equal(remaps, want) {
		t.Errorf("setup made the sources %q, want %q", remaps, want)
	}

	if out, errOut, code := callprobe(t, "", "setup"); code != 0 {
		t.Fatalf("setup again exited %d: %s%s", code, out, errOut)
	}
	if again := calls(t, log, "load-module"); len(again) != len(loads) {
		t.Errorf("setup run again loaded %q, want nothing more", again[len(loads):])
	}
	if got := lines(t, state); !slices.Contains(got, "7\tmodule-null-sink\tsink_name=other-sink") {
		t.Errorf("setup lost a module that is not callprobe's: %q", got)
	}
}

// setup -remove unloads callprobe's three modules, the source that reads
// callprobe-mic's monitor first, and no other.
func TestSetupRemoveUnloadsOnlyItsDevices(t *testing.T) {
	state, log := setupPactl(t)
	if out, errOut, code := callprobe(t, "", "setup"); code != 0 {
		t.Fatalf("setup exited %d: %s%s", code, out, errOut)
	}
	// The fake numbers the modules from 8 in the order setup loads them.
	remap := ""
	var sinks []string
	for _, l := range lines(t, state) {
		index, rest, _ := strings.Cut(l, "\t")
		switch {
		case strings.HasPrefix(rest, "module-remap-source"):
			remap = index
		case strings.Contains(rest, "sink_name=callprobe-"):
			sinks = append(sinks, index)
		}
	}
	if out, errOut, code := callprobe(t, "", "setup", "-remove"); code != 0 {
		t.Fatalf("setup -remove exited %d: %s%s", code, out, errOut)
	}
	var unloaded []string
	for _, call := range calls(t, log, "unload-module") {
		unloaded = append(unloaded, strings.Fields(call)[1])
	}
	if len(unloaded) != 3 || unloaded[0] != remap {
		t.Fatalf("setup -remove unloaded modules %q, want the remap source %s first, then the sinks %q", unloaded, remap, sinks)
	}
	rest := slices.Sorted(slices.Values(unloaded[1:]))
	if slices.Sort(sinks); !slices.Equal(rest, sinks) {
		t.Errorf("setup -remove unloaded %q after the source, want the sinks %q", rest, sinks)
	}
	if got := lines(t, state); !slices.Equal(got, []string{"7\tmodule-null-sink\tsink_name=other-sink"}) {
		t.Errorf("after setup -remove the modules are %q, want only the one that is not callprobe's", got)
	}
}

// fakeFFmpeg logs its arguments, one a line, and writes each .wav it is
// given as an output as ffmpeg does, a 48 kHz mono 16-bit PCM WAV file with
// ffmpeg's LIST chunk before its data: with FAKE_FFMPEG_WRITE=audio, 1000
// bytes of samples; with header, none, a file of 78 bytes. It exits
// FAKE_FFMPEG_EXIT, 0 by default.
const fakeFFmpeg = `printf '%s\n' "$@" >"$FAKE_FFMPEG_LOG"
le32() { # le32 N: N as 4 bytes, little-endian
	local b
	printf -v b '\\x%02x\\x%02x\\x%02x\\x%02x' $(($1 & 255)) $(($1 >> 8 & 255)) $(($1 >> 16 & 255)) $(($1 >> 24 & 255))
	printf "$b"
}
wav() { # wav FILE N: the WAV file, with N bytes of samples
	{
		printf 'RIFF'; le32 $((70 + $2)); printf 'WAVEfmt '; le32 16
		printf '\x01\x00\x01\x00'; le32 48000; le32 96000; printf '\x02\x00\x10\x00'
		printf 'LIST'; le32 26; printf 'INFOISFT'; le32 13; printf 'Lavf61.7.100\x00\x00'
		printf 'data'; le32 "$2"; printf '%*s' "$2" ''
	} >"$1"
}
input=no
for a in "$@"; do
	if [[ $input == yes ]]; then input=no; continue; fi
	[[ $a == -i ]] && input=yes
	[[ $a == *.wav ]] || continue
	case ${FAKE_FFMPEG_WRITE:-} in
	audio) wav "$a" 1000 ;;
	header) wav "$a" 0 ;;
	esac
done
exit "${FAKE_FFMPEG_EXIT:-0}"
`

// ffmpegStream is one input or output of an ffmpeg command line: its URL or
// file and the options given before it.
type ffmpegStream struct {
	url  string
	opts map[string]string
}

// ffmpegArgs reads an ffmpeg command line as ffmpeg does: the options before
// each -i are that input's, the options before each output file that
// output's. -hide_banner, -nostdin, -n and -y take no value.
func ffmpegArgs(args []string) (inputs, outputs []ffmpegStream) {
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-hide_banner" || a == "-nostdin" || a == "-n" || a == "-y":
		case a == "-i" && i+1 < len(args):
			inputs = append(inputs, ffmpegStream{args[i+1], opts})
			opts = map[string]string{}
			i++
		case strings.HasPrefix(a, "-") && i+1 < len(args):
			opts[a] = args[i+1]
			i++
		default:
			outputs = append(outputs, ffmpegStream{a, opts})
			opts = map[string]string{}
		}
	}
	return inputs, outputs
}

// setupFFmpeg puts the fake ffmpeg on PATH and returns its log's path.
func setupFFmpeg(t *testing.T) string {
	t.Helper()
	dir := fakePath(t, map[string]string{"ffmpeg": fakeFFmpeg})
	log := filepath.Join(dir, "ffmpeg-args")
	t.Setenv("FAKE_FFMPEG_LOG", log)
	return log
}

// play sends a file to Meet's microphone, callprobe-mic, by default, and to
// each device it is given at once (calibration 1 plays one click to the
// laptop's speaker and to its loopback). ffmpeg's PulseAudio output throws
// away what the server still buffers when its input ends (pa_stream_flush,
// then a disconnect with no drain: libavdevice/pulse_audio_enc.c at n6.1.1),
// which lost the last 2 s of each file in a run on a real server; so each
// output asks for a short buffer and pads the file with more silence than
// that buffer holds.
func TestPlaySendsTheFileToEachDevice(t *testing.T) {
	log := setupFFmpeg(t)
	file := writeWAV(t, t.TempDir(), "click.wav", 48000, make([]float64, 4800))
	for _, tc := range []struct {
		name    string
		to      []string
		devices []string // each output's -device; "" is the default sink
	}{
		{"Meet's microphone", nil, []string{"callprobe-mic"}},
		{"the speaker and the loopback", []string{"default", "callprobe-loopback"}, []string{"", "callprobe-loopback"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"play"}
			for _, d := range tc.to {
				args = append(args, "-to", d)
			}
			if out, errOut, code := callprobe(t, "", append(args, file)...); code != 0 {
				t.Fatalf("play exited %d: %s%s", code, out, errOut)
			}
			inputs, outputs := ffmpegArgs(lines(t, log))
			if len(inputs) != 1 || inputs[0].url != file {
				t.Errorf("ffmpeg's inputs are %v, want the one file %s", inputs, file)
			}
			var devices []string
			for _, o := range outputs {
				if o.opts["-f"] != "pulse" {
					t.Errorf("output %s has format %q, want pulse", o.url, o.opts["-f"])
				}
				devices = append(devices, o.opts["-device"])
				buffer, err := strconv.Atoi(o.opts["-buffer_duration"])
				if err != nil || buffer <= 0 {
					t.Errorf("output %s asks for a buffer of %q ms, want a set one", o.url, o.opts["-buffer_duration"])
				}
				pad, ok := strings.CutPrefix(o.opts["-af"], "apad=pad_dur=")
				seconds, err := strconv.ParseFloat(pad, 64)
				if !ok || err != nil || seconds*1000 <= float64(2*buffer) {
					t.Errorf("output %s filters %q, want apad=pad_dur= of more than twice its %d ms buffer", o.url, o.opts["-af"], buffer)
				}
			}
			if !slices.Equal(devices, tc.devices) {
				t.Errorf("ffmpeg plays to the devices %q, want %q", devices, tc.devices)
			}
		})
	}
	t.Run("a failed ffmpeg", func(t *testing.T) {
		t.Setenv("FAKE_FFMPEG_EXIT", "1")
		if _, _, code := callprobe(t, "", "play", file); code == 0 {
			t.Error("play exited 0 when ffmpeg failed")
		}
	})
}

// record writes, in one ffmpeg, Meet's speaker (callprobe-loopback's
// monitor) to loopback.wav, Meet's microphone (callprobe-mic's monitor) to
// virtual-mic.wav and the laptop's own microphone to room.wav, mono at
// 48 kHz, each for the time it is given.
func TestRecordWritesEachDeviceToItsFile(t *testing.T) {
	log := setupFFmpeg(t)
	t.Setenv("FAKE_FFMPEG_WRITE", "audio")
	for _, tc := range []struct {
		name  string
		extra []string
		want  map[string]string // file: the device it records
	}{
		{"the three devices", nil, map[string]string{
			"loopback.wav": "callprobe-loopback.monitor", "virtual-mic.wav": "callprobe-mic.monitor", "room.wav": "default",
		}},
		{"a named room microphone", []string{"-from", "room=alsa_input.usb-test"}, map[string]string{
			"loopback.wav": "callprobe-loopback.monitor", "virtual-mic.wav": "callprobe-mic.monitor", "room.wav": "alsa_input.usb-test",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "run")
			args := append([]string{"record", "-out", dir, "-for", "5s"}, tc.extra...)
			if out, errOut, code := callprobe(t, "", args...); code != 0 {
				t.Fatalf("record exited %d: %s%s", code, out, errOut)
			}
			inputs, outputs := ffmpegArgs(lines(t, log))
			for _, in := range inputs {
				if in.opts["-f"] != "pulse" || in.opts["-sample_rate"] != "48000" || in.opts["-channels"] != "1" {
					t.Errorf("input %s has options %v, want pulse, 48000 Hz, 1 channel", in.url, in.opts)
				}
				// With ffmpeg's default queue of 8 packets, a real run warned
				// "Thread message queue blocking" on two of the three devices: a
				// device whose packets wait can lose samples, and move its file's
				// time against the others'.
				if queue, err := strconv.Atoi(in.opts["-thread_queue_size"]); err != nil || queue < 1024 {
					t.Errorf("input %s has a thread queue of %q packets, want at least 1024", in.url, in.opts["-thread_queue_size"])
				}
			}
			got := map[string]string{}
			for _, o := range outputs {
				index, ok := strings.CutSuffix(o.opts["-map"], ":a")
				n, err := strconv.Atoi(index)
				if !ok || err != nil || n < 0 || n >= len(inputs) {
					t.Fatalf("output %s maps %q, want one input's audio", o.url, o.opts["-map"])
				}
				if o.opts["-t"] != "5.000" {
					t.Errorf("output %s lasts %q, want 5.000 s", o.url, o.opts["-t"])
				}
				if filepath.Dir(o.url) != dir {
					t.Errorf("output %s is outside %s", o.url, dir)
				}
				got[filepath.Base(o.url)] = inputs[n].url
			}
			if len(got) != len(tc.want) {
				t.Errorf("record writes %v, want %v", got, tc.want)
			}
			for file, device := range tc.want {
				if got[file] != device {
					t.Errorf("%s records %q, want %q", file, got[file], device)
				}
			}
		})
	}
}

// A recording that ffmpeg did not write, or wrote with no sample, or a failed
// ffmpeg, is no recording: record exits non-zero and names the file.
func TestRecordRefusesAnEmptyRecording(t *testing.T) {
	setupFFmpeg(t)
	t.Run("no file written", func(t *testing.T) {
		out, errOut, code := callprobe(t, "", "record", "-out", filepath.Join(t.TempDir(), "run"), "-for", "5s")
		if code == 0 {
			t.Errorf("record exited 0 with no file written:\n%s", out)
		}
		if !strings.Contains(errOut, "loopback.wav") {
			t.Errorf("stderr does not name loopback.wav, which ffmpeg did not write:\n%s", errOut)
		}
	})
	// ffmpeg writes a LIST chunk before the data, so its header is 78 bytes,
	// not 44: the file's size does not say whether it holds a sample.
	t.Run("a header and no sample", func(t *testing.T) {
		t.Setenv("FAKE_FFMPEG_WRITE", "header")
		dir := filepath.Join(t.TempDir(), "run")
		out, errOut, code := callprobe(t, "", "record", "-out", dir, "-for", "5s")
		if info, err := os.Stat(filepath.Join(dir, "loopback.wav")); err != nil || info.Size() != 78 {
			t.Fatalf("the fake ffmpeg wrote loopback.wav as %v, %v; want a header of 78 bytes", info, err)
		}
		if code == 0 {
			t.Errorf("record exited 0 when each file holds a WAV header and no sample:\n%s", out)
		}
		if !strings.Contains(errOut, "loopback.wav") {
			t.Errorf("stderr does not name loopback.wav, which holds no sample:\n%s", errOut)
		}
	})
	t.Run("a failed ffmpeg", func(t *testing.T) {
		t.Setenv("FAKE_FFMPEG_WRITE", "audio")
		t.Setenv("FAKE_FFMPEG_EXIT", "1")
		if _, _, code := callprobe(t, "", "record", "-out", filepath.Join(t.TempDir(), "run"), "-for", "5s"); code == 0 {
			t.Error("record exited 0 when ffmpeg failed")
		}
	})
}
