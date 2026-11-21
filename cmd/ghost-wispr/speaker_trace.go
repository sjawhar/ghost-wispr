package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/speakertrace"
)

// runSpeakerTraceCommand is `ghost-wispr speaker-trace [-slow D] FILE...`: for
// each capture the speaker trace saved on an underrun (speaker_trace_dir), it
// prints the mixer's cycles slower than -slow and what kept the mixer from the
// device in each, longest first. It exits 1 when a file cannot be read, and 2
// when a capture holds no slow cycle, which shows nothing.
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
		slices.SortStableFunc(cycles, func(a, b speakertrace.Cycle) int { return int(b.Length() - a.Length()) })
		for i := range cycles {
			c := &cycles[i]
			_, _ = fmt.Fprintf(stdout, "  at %v, %v: %s\n", c.Start.Round(time.Millisecond), c.Length().Round(100*time.Microsecond), c.Classify())
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
