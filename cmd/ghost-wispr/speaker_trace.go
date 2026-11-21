package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/speakertrace"
)

// runSpeakerTraceCommand is `ghost-wispr speaker-trace [-slow D] FILE...`: for
// each capture the speaker trace saved on an underrun (speaker_trace_dir), it
// prints the mixer's cycles slower than -slow and what kept the mixer from the
// device in each, longest first. Each cycle's start and end are given on the
// wall clock, in UTC with microseconds, as `journalctl --utc -o
// short-iso-precise` prints, from the capture's own clock snapshots, and also
// as time into the capture, which is not time since the process started. A
// wall-clock time the snapshots cannot pin to one instant (a clock step
// between them, or no snapshot before it) is printed qualified, never as a
// plain time (speakertrace.Wall). It exits 1 when a file cannot be read, and
// 2 when a capture holds no slow cycle, which shows nothing.
func runSpeakerTraceCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("speaker-trace", flag.ContinueOnError)
	flags.SetOutput(stderr)
	slow := flags.Duration("slow", 40*time.Millisecond, "the mixer's cycles longer than this are printed: half the Jabra's 80 ms buffer")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if flags.NArg() == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: ghost-wispr speaker-trace [-slow 40ms] FILE...")
		return 1
	}
	code := 0
	for _, path := range flags.Args() {
		f, err := os.Open(path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "speaker-trace: %v\n", err)
			return 1
		}
		cycles, err := speakertrace.Read(f, *slow)
		_ = f.Close()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "speaker-trace: %s: %v\n", path, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "%s: %d cycles of the mixer longer than %v\n", path, len(cycles), *slow)
		if len(cycles) == 0 {
			_, _ = fmt.Fprintln(stdout, "UNKNOWN: no slow cycle in this capture, so it shows nothing of the underrun")
			code = max(code, 2)
			continue
		}
		if !cycles[0].StartWall.IsZero() {
			_, _ = fmt.Fprintln(stdout, "  Wall-clock times are projected from the latest clock snapshot before each (one each trace generation), assuming the clock did not step since; a step the next snapshot shows is printed as one.")
		}
		slices.SortStableFunc(cycles, func(a, b speakertrace.Cycle) int { return int(b.Length() - a.Length()) })
		for i := range cycles {
			c := &cycles[i]
			_, _ = fmt.Fprintf(stdout, "  %s, %v: %s\n", cycleTimes(c), c.Length().Round(100*time.Microsecond), c.Classify())
			buckets := make([]string, 0, len(c.Spent))
			for b := range c.Spent {
				buckets = append(buckets, b)
			}
			slices.SortFunc(buckets, func(a, b string) int { return int(c.Spent[b] - c.Spent[a]) })
			for _, b := range buckets {
				_, _ = fmt.Fprintf(stdout, "    %-40s %v\n", b, c.Spent[b].Round(100*time.Microsecond))
			}
		}
	}
	return code
}

// wallLayout is how the command prints a wall-clock time: UTC, to the
// microsecond, as `journalctl --utc -o short-iso-precise` does.
const wallLayout = "2006-01-02T15:04:05.000000Z07:00"

// cycleTimes is a cycle's start and end on the wall clock, each qualified
// when the snapshots cannot pin it, and its start into the capture; a capture
// without a clock snapshot gives the latter only.
func cycleTimes(c *speakertrace.Cycle) string {
	into := fmt.Sprintf("%v into the capture", c.Start.Round(time.Millisecond))
	if c.StartWall.IsZero() {
		return "at " + into + " (no wall-clock time in this capture)"
	}
	notes := []string{into}
	for _, w := range []speakertrace.Wall{c.StartWall, c.EndWall} {
		if w.Note != "" && !slices.Contains(notes, w.Note) {
			notes = append(notes, w.Note)
		}
	}
	return fmt.Sprintf("from %s to %s (%s)", wallTime(c.StartWall), wallTime(c.EndWall), strings.Join(notes, "; "))
}

// wallTime is one wall-clock time, or the interval it lies in.
func wallTime(w speakertrace.Wall) string {
	if w.From.Equal(w.To) {
		return w.From.UTC().Format(wallLayout)
	}
	return "between " + w.From.UTC().Format(wallLayout) + " and " + w.To.UTC().Format(wallLayout)
}
