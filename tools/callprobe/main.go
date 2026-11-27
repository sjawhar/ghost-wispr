// Command callprobe is the measuring laptop's instrument for a test call
// (INFRA-5904): a Linux laptop with PulseAudio or PipeWire that joins the
// call as a remote participant. Its Meet microphone is a virtual device fed
// from files, its Meet speaker a loopback it records, and its own microphone
// records the room, all three on its one audio clock.
//
//	callprobe setup [-remove]
//	callprobe play [-to DEVICE]... FILE
//	callprobe record -out DIR -for DURATION [-from NAME=DEVICE]...
//	callprobe analyze ends|clicks|delay|echo [FLAGS]
//	callprobe pings < tailscale ping lines
//
// setup makes the two devices with pactl (setup-linux.sh): callprobe-mic,
// whose monitor is Meet's microphone through the source callprobe-mic-in,
// and callprobe-loopback, Meet's speaker. play sends a file to devices with
// ffmpeg, callprobe-mic by default. record writes, with one ffmpeg,
// loopback.wav (callprobe-loopback's monitor: what Meet plays),
// virtual-mic.wav (callprobe-mic's monitor: what Meet hears from the laptop)
// and room.wav (the laptop's own microphone).
//
// analyze reads 16-bit PCM WAV files and prints key=value lines, first the
// duration it read of each, and refuses a window a file does not hold:
// ends, each sound's start and end; clicks, each click's time and, against
// -ref, each click's delay, after calibration 1's offset with -offset, with
// their median and p90; delay, the lag of -ref's copy in -in by
// cross-correlation; echo, that copy's level and the recording's noise floor
// against -ref, and with -second-after a second, later copy's level against
// the first's. An analysis that finds nothing says so, a copy below the
// noise floor reads UNKNOWN, never a level, and no value is NaN or an
// infinity.
//
// pings reads `tailscale ping` lines on stdin and prints, for each target,
// the p50 and p99 of its reply times; a target that never replied makes it
// exit 1, named.
package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage:
  callprobe setup [-remove]
  callprobe play [-to DEVICE]... FILE
  callprobe record -out DIR -for DURATION [-from NAME=DEVICE]...
  callprobe analyze ends|clicks|delay|echo [FLAGS]
  callprobe pings < tailscale ping lines
`

// run is callprobe with args, reading stdin, and returns its exit code: 0
// done, 1 failed or refused, 2 a bad command line.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "play":
		return runPlay(args[1:], stdout, stderr)
	case "record":
		return runRecord(args[1:], stdout, stderr)
	case "analyze":
		return runAnalyze(args[1:], stdout, stderr)
	case "pings":
		return runPings(args[1:], stdin, stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "callprobe: no subcommand %q\n%s", args[0], usage)
	return 2
}

// ms is d in milliseconds, to a tenth.
func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
}

// db is a level in dB, to a tenth. The level of no energy at all, minus
// infinity, is "silence", and a level that is no number is UNKNOWN: no
// output holds NaN or an infinity, which a reader's comparison could pass.
func db(level float64) string {
	switch {
	case math.IsInf(level, -1):
		return "silence"
	case math.IsNaN(level) || math.IsInf(level, 1):
		return "UNKNOWN"
	}
	return strconv.FormatFloat(level, 'f', 1, 64)
}

// now is the laptop's clock, in UTC to the microsecond, for the lines that say
// when a play or a recording started.
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000Z") }
