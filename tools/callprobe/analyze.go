package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/sjawhar/ghost-wispr/tools/callprobe/analyze"
)

const analyzeUsage = `usage:
  callprobe analyze ends -in FILE [-above 10] [-gap 50ms] [-min 5ms]
  callprobe analyze clicks -in FILE [-ref FILE [-offset DURATION]] [-above 20] [-quiet 100ms] [-max-lag 1s]
  callprobe analyze delay -ref FILE -in FILE [-max-lag 5s] [-path 10ms]
  callprobe analyze echo -ref FILE -in FILE [-max-lag 5s] [-path 10ms] [-second-after DURATION]
each with [-channel N] [-start DURATION] [-for DURATION]; times are from the file's start
`

// errUsage is a bad command line, already explained on stderr.
var errUsage = errors.New("usage")

func runAnalyze(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, analyzeUsage)
		return 2
	}
	analyses := map[string]func([]string, io.Writer, io.Writer) error{
		"ends": analyzeEnds, "clicks": analyzeClicks, "delay": analyzeDelay, "echo": analyzeEcho,
	}
	analysis, ok := analyses[args[0]]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "callprobe analyze: no analysis %q\n%s", args[0], analyzeUsage)
		return 2
	}
	switch err := analysis(args[1:], stdout, stderr); {
	case errors.Is(err, errUsage):
		return 2
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "callprobe analyze %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// files are the flags that name what an analysis reads.
type files struct {
	in, ref string
	read    analyze.ReadOptions
}

func (f *files) flags(name string, stderr io.Writer, ref bool) *flag.FlagSet {
	fs := flag.NewFlagSet("callprobe analyze "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.in, "in", "", "the recording, a 16-bit PCM WAV file")
	if ref {
		fs.StringVar(&f.ref, "ref", "", "the reference the recording holds a copy of, a 16-bit PCM WAV file")
	}
	fs.IntVar(&f.read.Channel, "channel", 0, "the channel to read, from 1; 0 reads the mean of every channel")
	fs.DurationVar(&f.read.Start, "start", 0, "read from this far into each file")
	fs.DurationVar(&f.read.Length, "for", 0, "read this much of each file, which must hold it; 0 reads to its end")
	return fs
}

// parse parses args into fs, and needs -in and, with needRef, -ref.
func (f *files) parse(fs *flag.FlagSet, args []string, needRef bool) error {
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	need := "-in"
	if needRef {
		need = "-in and -ref"
	}
	if fs.NArg() > 0 || f.in == "" || (needRef && f.ref == "") {
		_, _ = fmt.Fprintf(fs.Output(), "%s: give %s and no other argument\n", fs.Name(), need)
		return errUsage
	}
	return nil
}

// printRead prints how long a stretch of each file an analysis read, from
// -start: a file shorter than the window it was asked for is refused, so
// this is the window.
func printRead(w io.Writer, in analyze.Audio, ref *analyze.Audio) {
	if ref == nil {
		_, _ = fmt.Fprintf(w, "in_duration_ms=%s\n", ms(in.Duration()))
		return
	}
	_, _ = fmt.Fprintf(w, "in_duration_ms=%s ref_duration_ms=%s\n", ms(in.Duration()), ms(ref.Duration()))
}

func analyzeEnds(args []string, stdout, stderr io.Writer) error {
	var f files
	var o analyze.SoundOptions
	fs := f.flags("ends", stderr, false)
	fs.Float64Var(&o.Above, "above", 10, "dB by which a sound's 1 ms level stands above the noise floor")
	fs.DurationVar(&o.Gap, "gap", 50*time.Millisecond, "a quieter stretch shorter than this joins the sounds on each side")
	fs.DurationVar(&o.Min, "min", 5*time.Millisecond, "a sound shorter than this is none")
	if err := f.parse(fs, args, false); err != nil {
		return err
	}
	a, err := analyze.ReadWAVFile(f.in, f.read)
	if err != nil {
		return err
	}
	found, err := analyze.Ends(a, o)
	if err != nil {
		return err
	}
	printRead(stdout, a, nil)
	_, _ = fmt.Fprintf(stdout, "noise_floor_dbfs=%s threshold_dbfs=%s\n", db(found.FloorDBFS), db(found.ThresholdDBFS))
	for _, s := range found.Sounds {
		_, _ = fmt.Fprintf(stdout, "sound start_ms=%s end_ms=%s peak_dbfs=%s\n", ms(s.Start), ms(s.End), db(s.PeakDBFS))
	}
	if len(found.Sounds) == 0 {
		_, _ = fmt.Fprintf(stdout, "sounds=0 (no sound stands %s dB above the noise floor)\n", db(o.Above))
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "sounds=%d\n", len(found.Sounds))
	return nil
}

func analyzeClicks(args []string, stdout, stderr io.Writer) error {
	var f files
	var o analyze.ClickOptions
	var maxLag, offset time.Duration
	offsetSet := false
	fs := f.flags("clicks", stderr, true)
	fs.Float64Var(&o.Above, "above", 20, "dB by which a click's sample stands above the noise floor")
	fs.DurationVar(&o.Quiet, "quiet", 100*time.Millisecond, "the quiet before a click")
	fs.DurationVar(&maxLag, "max-lag", time.Second, "with -ref, how far either side of a reference click, moved by -offset, its copy is looked for")
	fs.Func("offset", "with -ref, calibration 1's offset: a time in -ref plus this is the same moment in -in. Each delay is then the path's alone, and one below 0 is refused", func(s string) error {
		var err error
		offset, err = time.ParseDuration(s)
		offsetSet = true
		return err
	})
	if err := f.parse(fs, args, false); err != nil {
		return err
	}
	if offsetSet && f.ref == "" {
		_, _ = fmt.Fprintf(stderr, "%s: give -offset only with -ref\n", fs.Name())
		return errUsage
	}
	find := func(path string) (analyze.Audio, analyze.Clicks, error) {
		a, err := analyze.ReadWAVFile(path, f.read)
		if err != nil {
			return analyze.Audio{}, analyze.Clicks{}, err
		}
		c, err := analyze.FindClicks(a, o)
		return a, c, err
	}
	inAudio, in, err := find(f.in)
	if err != nil {
		return err
	}
	if f.ref == "" {
		printRead(stdout, inAudio, nil)
		_, _ = fmt.Fprintf(stdout, "noise_floor_dbfs=%s threshold_dbfs=%s\n", db(in.FloorDBFS), db(in.ThresholdDBFS))
		for _, at := range in.At {
			_, _ = fmt.Fprintf(stdout, "click at_ms=%s\n", ms(at))
		}
		if len(in.At) == 0 {
			_, _ = fmt.Fprintf(stdout, "clicks=0 (no sample stands %s dB above the noise floor after %v of quiet)\n", db(o.Above), o.Quiet)
			return nil
		}
		_, _ = fmt.Fprintf(stdout, "clicks=%d\n", len(in.At))
		return nil
	}
	refAudio, ref, err := find(f.ref)
	if err != nil {
		return err
	}
	pairs, err := analyze.PairClicks(ref.At, in.At, offset, maxLag)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(pairs, func(p analyze.ClickPair) bool { return p.Delay < 0 }); offsetSet && i >= 0 {
		p := pairs[i]
		return fmt.Errorf("the click of -ref at %v pairs with the click of -in at %v, %v before it with the offset of %v: "+
			"a copy cannot come before its click, so the offset is wrong, or the copy is more than -max-lag late and paired with the next click",
			p.Ref, p.In, -p.Delay, offset)
	}
	printRead(stdout, inAudio, &refAudio)
	for _, c := range []struct {
		name string
		analyze.Clicks
	}{{"ref", ref}, {"in", in}} {
		_, _ = fmt.Fprintf(stdout, "%s noise_floor_dbfs=%s threshold_dbfs=%s clicks=%d\n", c.name, db(c.FloorDBFS), db(c.ThresholdDBFS), len(c.At))
	}
	moved := ""
	if offsetSet {
		_, _ = fmt.Fprintf(stdout, "offset_ms=%s (each delay_ms is -in's time less -ref's less the offset)\n", ms(offset))
		moved = ", moved by the offset"
	}
	delays := make([]time.Duration, 0, len(pairs))
	for _, p := range pairs {
		_, _ = fmt.Fprintf(stdout, "pair ref_ms=%s in_ms=%s delay_ms=%s\n", ms(p.Ref), ms(p.In), ms(p.Delay))
		delays = append(delays, p.Delay)
	}
	unpaired := fmt.Sprintf("unpaired_ref=%d unpaired_in=%d", len(ref.At)-len(pairs), len(in.At)-len(pairs))
	if len(pairs) == 0 {
		_, _ = fmt.Fprintf(stdout, "pairs=0 %s (no click of -in is within %v of a click of -ref%s)\n", unpaired, maxLag, moved)
		return nil
	}
	slices.Sort(delays)
	_, _ = fmt.Fprintf(stdout, "pairs=%d %s median_ms=%s p90_ms=%s spread_ms=%s\n", len(pairs), unpaired,
		ms(analyze.Percentile(delays, 50)), ms(analyze.Percentile(delays, 90)), ms(delays[len(delays)-1]-delays[0]))
	return nil
}

// findCopy reads the two files of a delay or echo analysis, finds -ref's
// copy in -in, and prints how much of each it read.
func findCopy(name string, args []string, stdout, stderr io.Writer) (analyze.Copy, analyze.CopyOptions, error) {
	var f files
	var o analyze.CopyOptions
	fs := f.flags(name, stderr, true)
	fs.DurationVar(&o.MaxLag, "max-lag", 5*time.Second, "how far before and after -ref the copy is looked for")
	fs.DurationVar(&o.Path, "path", 10*time.Millisecond, "the length of echo path fitted around the copy's strongest lag")
	if name == "echo" {
		fs.DurationVar(&o.SecondAfter, "second-after", 0,
			"also find a second copy at least this long after the first, the direct copy, and give its level against the direct one; at least -path")
	}
	if err := f.parse(fs, args, true); err != nil {
		return analyze.Copy{}, o, err
	}
	ref, err := analyze.ReadWAVFile(f.ref, f.read)
	if err != nil {
		return analyze.Copy{}, o, err
	}
	in, err := analyze.ReadWAVFile(f.in, f.read)
	if err != nil {
		return analyze.Copy{}, o, err
	}
	c, err := analyze.FindCopy(ref, in, o)
	if err != nil {
		return analyze.Copy{}, o, err
	}
	printRead(stdout, in, &ref)
	return c, o, nil
}

func analyzeDelay(args []string, stdout, stderr io.Writer) error {
	c, o, err := findCopy("delay", args, stdout, stderr)
	switch {
	case err != nil:
		return err
	case c.Silent:
		_, _ = fmt.Fprintln(stdout, "delay=none (-in is digital silence where it overlaps -ref)")
	case !c.Found:
		_, _ = fmt.Fprintf(stdout, "delay=none (no copy of -ref stands out of the cross-correlation's noise within ±%v)\n", o.MaxLag)
	default:
		_, _ = fmt.Fprintf(stdout, "delay_ms=%s\n", ms(c.Lag))
	}
	return nil
}

func analyzeEcho(args []string, stdout, stderr io.Writer) error {
	c, o, err := findCopy("echo", args, stdout, stderr)
	switch {
	case err != nil:
		return err
	case o.SecondAfter > 0:
		printSecondCopy(stdout, c, o)
	case c.Silent:
		_, _ = fmt.Fprintln(stdout, "echo_db=UNKNOWN (-in is digital silence where it overlaps -ref: it cannot show that an echo is absent)")
	case !c.Found:
		_, _ = fmt.Fprintf(stdout, "echo=none noise_floor_db=%s (dB against -ref; no copy of -ref stands out of the cross-correlation's noise within ±%v: the echo is at most the floor)\n",
			db(c.FloorDB), o.MaxLag)
	case c.BelowFloor():
		_, _ = fmt.Fprintf(stdout, "delay_ms=%s echo_db=UNKNOWN noise_floor_db=%s (dB against -ref; a copy of -ref is in -in below its noise floor: the echo is at most the floor)\n",
			ms(c.Lag), db(c.FloorDB))
	default:
		_, _ = fmt.Fprintf(stdout, "delay_ms=%s echo_db=%s noise_floor_db=%s (dB against -ref)\n", ms(c.Lag), db(c.LevelDB), db(c.FloorDB))
	}
	return nil
}

// printSecondCopy prints what echo -second-after found: the direct copy,
// the strongest, then the strongest copy at least -second-after later, each
// against -ref, and the second copy and the floor against the direct one. A
// second copy the fit cannot tell prints copy=UNKNOWN and no floor, which
// would be no bound on it.
func printSecondCopy(w io.Writer, c analyze.Copy, o analyze.CopyOptions) {
	switch {
	case c.Silent:
		_, _ = fmt.Fprintln(w, "direct_db=UNKNOWN (-in is digital silence where it overlaps -ref: it cannot show that a copy is absent)")
		return
	case !c.Found:
		_, _ = fmt.Fprintf(w, "direct=none noise_floor_db=%s (dB against -ref; no copy of -ref stands out of the cross-correlation's noise within ±%v: no direct copy to measure a second against)\n",
			db(c.FloorDB), o.MaxLag)
		return
	case c.BelowFloor():
		_, _ = fmt.Fprintf(w, "direct_delay_ms=%s direct_db=UNKNOWN noise_floor_db=%s (dB against -ref; the direct copy is below the noise floor: no second copy can be measured against it)\n",
			ms(c.Lag), db(c.FloorDB))
		return
	}
	direct := fmt.Sprintf("direct_delay_ms=%s direct_db=%s", ms(c.Lag), db(c.LevelDB))
	floor := fmt.Sprintf("noise_floor_db=%s floor_vs_direct_db=%s", db(c.FloorDB), db(c.FloorDB-c.LevelDB))
	switch s := c.Second; {
	case s.Unknown != "":
		_, _ = fmt.Fprintf(w, "%s copy=UNKNOWN (dB against -ref; %s: no level of a copy of -ref %v or more after the direct one within ±%v is known, and the floor is no bound on it)\n",
			direct, s.Unknown, o.SecondAfter, o.MaxLag)
	case !s.Found:
		_, _ = fmt.Fprintf(w, "%s copy=none %s (dB against -ref, and against the direct copy; no copy of -ref %v or more after the direct one stands out of the cross-correlation's noise within ±%v: the copy is at most the floor)\n",
			direct, floor, o.SecondAfter, o.MaxLag)
	case c.SecondBelowFloor():
		_, _ = fmt.Fprintf(w, "%s copy_delay_ms=%s copy_db=UNKNOWN copy_vs_direct_db=UNKNOWN %s (dB against -ref, and against the direct copy; a second copy is in -in below its noise floor: the copy is at most the floor)\n",
			direct, ms(s.Lag), floor)
	default:
		_, _ = fmt.Fprintf(w, "%s copy_delay_ms=%s copy_db=%s copy_vs_direct_db=%s %s (dB against -ref, and against the direct copy)\n",
			direct, ms(s.Lag), db(s.LevelDB), db(s.LevelDB-c.LevelDB), floor)
	}
}
