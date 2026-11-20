// Command measure-wake measures the paused listener's spotter on stored
// audio, never on a room: its false starts per hour of speech that holds no
// start phrase, its hits on recordings of the phrase, and its CPU per second
// of audio (INFRA-4592, checks c-listener-accuracy and c-listener-cpu). It
// needs a build with the spotter: go run -tags kws ./cmd/measure-wake.
//
//	measure-wake -mode listener -audio DIR [-phrases DIR] [-splice DIR]
//
// -audio is a tree of 16 kHz mono 16-bit WAV files of speech with no start
// phrase, such as LibriSpeech or synthetic speech. The files of each
// directory are streamed in order as one continuous stream, in 100 ms
// buffers, as the microphone delivers them; each directory starts a new
// stream. Every phrase the listener accepts in it is a false start.
//
// -splice is a directory of start-phrase clips, spliced into the -audio
// streams once every -splice-every of audio: the control that shows the false
// start count comes from an instrument that can fire. A wake within 2 s of a
// spliced clip's end counts as a spliced hit, not a false start, and the
// spliced audio is not counted in the hours.
//
// -phrases is a directory whose subdirectories are cells (a person at a
// distance), each holding WAV files of one start phrase each; each clip is
// played between seconds of silence, and is a hit if the listener accepts it.
//
// The run prints key=value lines, then a verdict: PASS when it read at least
// -min-hours of audio with fewer than -limit false starts per hour, and, with
// -phrases, at least -min-hit-rate of the phrases, every cell holding at least
// -min-cell clips; FAIL otherwise; UNKNOWN when it read too little to judge,
// or when a splice control heard none of its clips.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/wake/local"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newDetector))
}

type options struct {
	mode        string
	audio       string
	phrases     string
	splice      string
	spliceEvery time.Duration
	boost       float64
	threshold   float64
	decoys      string
	stage2      bool
	parallel    int
	minHours    float64
	limit       float64
	minHitRate  float64
	minCell     int
	// detector makes each detector the run uses: newDetector, the
	// listener's own.
	detector func(options) (*local.Detector, error)
}

func run(args []string, stdout, stderr io.Writer, detector func(options) (*local.Detector, error)) int {
	o := options{detector: detector}
	flags := flag.NewFlagSet("measure-wake", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.mode, "mode", "listener", "what to measure: listener")
	flags.StringVar(&o.audio, "audio", "", "tree of WAV speech with no start phrase")
	flags.StringVar(&o.phrases, "phrases", "", "directory of cells, each a directory of start-phrase clips")
	flags.StringVar(&o.splice, "splice", "", "directory of start-phrase clips to splice into -audio, as a control")
	flags.DurationVar(&o.spliceEvery, "splice-every", 10*time.Minute, "audio between two spliced clips")
	flags.Float64Var(&o.boost, "boost", local.DefaultThresholds.Boost, "stage 1's boost")
	flags.Float64Var(&o.threshold, "threshold", local.DefaultThresholds.Threshold, "stage 1's threshold")
	flags.StringVar(&o.decoys, "decoys", "", `"stop": add the stop-recording decoy lines to stage 1`)
	flags.BoolVar(&o.stage2, "stage2", true, "confirm each stage-1 hit with the recognizer")
	flags.IntVar(&o.parallel, "parallel", 1, "detectors that share -audio's streams, each on its own CPU")
	flags.Float64Var(&o.minHours, "min-hours", 10, "the least audio a verdict rests on")
	flags.Float64Var(&o.limit, "limit", 0.5, "false starts per hour the verdict allows, exclusive")
	flags.Float64Var(&o.minHitRate, "min-hit-rate", 0.9, "the share of -phrases the verdict needs")
	flags.IntVar(&o.minCell, "min-cell", 10, "the fewest clips a cell of -phrases may hold")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if o.mode != "listener" {
		_, _ = fmt.Fprintf(stderr, "measure-wake: -mode %q: only listener is measured here\n", o.mode)
		return 2
	}
	if o.audio == "" && o.phrases == "" {
		_, _ = fmt.Fprintln(stderr, "measure-wake: give -audio, -phrases or both")
		return 2
	}
	if o.parallel < 1 {
		_, _ = fmt.Fprintf(stderr, "measure-wake: -parallel %d: at least 1\n", o.parallel)
		return 2
	}
	verdict, err := measure(o, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "measure-wake: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, verdict)
	if verdict != "PASS" {
		return 1
	}
	return 0
}

func newDetector(o options) (*local.Detector, error) {
	opts := local.Options{Thresholds: local.Thresholds{Boost: o.boost, Threshold: o.threshold}, NoStageTwo: !o.stage2}
	switch o.decoys {
	case "":
	case "stop":
		opts.Decoys = local.StopDecoys
	default:
		return nil, fmt.Errorf("-decoys %q: only stop", o.decoys)
	}
	return local.NewDetectorWith(opts)
}

// measure runs the listener's detector over the audio and the phrases and
// returns the verdict.
func measure(o options, out io.Writer) (string, error) {
	_, _ = fmt.Fprintf(out, "boost=%g threshold=%g stage2=%t decoys=%q parallel=%d\n", o.boost, o.threshold, o.stage2, o.decoys, o.parallel)
	verdicts := []string{}
	if o.audio != "" {
		v, err := measureFalseStarts(o, out)
		if err != nil {
			return "", err
		}
		verdicts = append(verdicts, v)
	}
	if o.phrases != "" {
		v, err := measurePhrases(o, out)
		if err != nil {
			return "", err
		}
		verdicts = append(verdicts, v)
	}
	switch {
	case slices.Contains(verdicts, "FAIL"):
		return "FAIL", nil
	case slices.Contains(verdicts, "UNKNOWN"):
		return "UNKNOWN", nil
	default:
		return "PASS", nil
	}
}

// streams lists the WAV files of root by directory, each directory's files
// sorted: one stream each.
func streams(root string) ([][]string, error) {
	byDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".wav") {
			byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	out := make([][]string, 0, len(dirs))
	for _, dir := range dirs {
		files := byDir[dir]
		sort.Strings(files)
		out = append(out, files)
	}
	return out, nil
}

// feed gives pcm to d in 100 ms buffers, and calls heard with the sample
// offset, within pcm, of each buffer that ended an accepted phrase.
func feed(d *local.Detector, pcm []int16, heard func(at int)) {
	const buffer = local.SampleRate / 10
	for at := 0; at < len(pcm); at += buffer {
		end := min(at+buffer, len(pcm))
		if d.Write(pcm[at:end]) {
			heard(end)
		}
	}
}

// cpuTime is the process's user and system CPU time.
func cpuTime() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// speechCounts are what one detector counted on its share of -audio.
type speechCounts struct {
	files, samples                int
	falseStarts, stageOneInSpeech int
	splicedClips, splicedHeard    int
}

func (c *speechCounts) add(o speechCounts) {
	c.files += o.files
	c.samples += o.samples
	c.falseStarts += o.falseStarts
	c.stageOneInSpeech += o.stageOneInSpeech
	c.splicedClips += o.splicedClips
	c.splicedHeard += o.splicedHeard
}

// countStreams runs d over every -parallel'th stream of all, from the
// worker'th, splicing a clip of spliced in once every spliceEvery samples of
// speech, straight after the speech, as a person says it after others talk.
// Each stream ends with 2 s of silence, not counted in the hours, in which a
// phrase at its very end is heard, as it would be in a stream that goes on.
func countStreams(d *local.Detector, all [][]string, worker, workers int, spliced [][]int16, spliceEvery int) (speechCounts, error) {
	var c speechCounts
	sinceSplice := 0
	silence := make([]int16, 2*local.SampleRate)
	speech := func(pcm []int16) {
		before := d.StageOneHits()
		feed(d, pcm, func(int) { c.falseStarts++ })
		c.stageOneInSpeech += d.StageOneHits() - before
	}
	for i := worker; i < len(all); i += workers {
		d.Reset()
		for _, file := range all[i] {
			pcm, err := local.ReadWAV(file)
			if err != nil {
				return c, err
			}
			c.files++
			c.samples += len(pcm)
			speech(pcm)
			sinceSplice += len(pcm)
			if len(spliced) > 0 && sinceSplice >= spliceEvery {
				sinceSplice = 0
				clip := spliced[(c.splicedClips*workers+worker)%len(spliced)]
				c.splicedClips++
				// The clip, then 2 s of silence, inside which its wake must
				// come: neither is counted in the hours.
				heard := false
				feed(d, slices.Concat(clip, silence), func(int) { heard = true })
				if heard {
					c.splicedHeard++
				}
			}
		}
		speech(silence)
	}
	return c, nil
}

func measureFalseStarts(o options, out io.Writer) (string, error) {
	all, err := streams(o.audio)
	if err != nil {
		return "", err
	}
	var spliced [][]int16
	if o.splice != "" {
		clips, err := filepath.Glob(filepath.Join(o.splice, "*.wav"))
		if err != nil {
			return "", err
		}
		sort.Strings(clips)
		for _, clip := range clips {
			pcm, err := local.ReadWAV(clip)
			if err != nil {
				return "", err
			}
			spliced = append(spliced, pcm)
		}
		if len(spliced) == 0 {
			return "", fmt.Errorf("-splice %s holds no WAV clip", o.splice)
		}
	}
	spliceEvery := int(o.spliceEvery.Seconds() * local.SampleRate)
	detectors := make([]*local.Detector, o.parallel)
	for i := range detectors {
		d, err := o.detector(o)
		if err != nil {
			return "", err
		}
		defer d.Close()
		detectors[i] = d
	}
	started := cpuTime()
	var (
		mu    sync.Mutex
		total speechCounts
		first error
		wg    sync.WaitGroup
	)
	for worker, d := range detectors {
		wg.Go(func() {
			c, err := countStreams(d, all, worker, o.parallel, spliced, spliceEvery)
			mu.Lock()
			defer mu.Unlock()
			total.add(c)
			if first == nil {
				first = err
			}
		})
	}
	wg.Wait()
	if first != nil {
		return "", first
	}
	used := cpuTime() - started
	hours := float64(total.samples) / local.SampleRate / 3600
	perHour, stageOnePerHour := 0.0, 0.0
	if hours > 0 {
		perHour = float64(total.falseStarts) / hours
		stageOnePerHour = float64(total.stageOneInSpeech) / hours
	}
	cpuShare := 0.0
	if total.samples > 0 {
		cpuShare = used.Seconds() / (float64(total.samples) / local.SampleRate)
	}
	_, _ = fmt.Fprintf(out, "audio_files=%d audio_hours=%.3f false_starts=%d false_starts_per_hour=%.3f\n",
		total.files, hours, total.falseStarts, perHour)
	_, _ = fmt.Fprintf(out, "stage1_hits_in_speech=%d stage1_hits_per_hour=%.3f\n", total.stageOneInSpeech, stageOnePerHour)
	_, _ = fmt.Fprintf(out, "cpu_seconds_per_audio_second=%.4f\n", cpuShare)
	if len(spliced) > 0 {
		_, _ = fmt.Fprintf(out, "spliced_clips=%d spliced_heard=%d\n", total.splicedClips, total.splicedHeard)
	}
	switch {
	case total.files == 0:
		_, _ = fmt.Fprintf(out, "UNKNOWN: no WAV file read under %s\n", o.audio)
		return "UNKNOWN", nil
	case len(spliced) > 0 && total.splicedHeard == 0:
		_, _ = fmt.Fprintln(out, "UNKNOWN: the listener heard none of the spliced phrases, so a count of 0 false starts proves nothing")
		return "UNKNOWN", nil
	case hours < o.minHours:
		_, _ = fmt.Fprintf(out, "UNKNOWN: %.3f h of audio, fewer than %g h\n", hours, o.minHours)
		return "UNKNOWN", nil
	case perHour < o.limit:
		return "PASS", nil
	default:
		return "FAIL", nil
	}
}

func measurePhrases(o options, out io.Writer) (string, error) {
	d, err := o.detector(o)
	if err != nil {
		return "", err
	}
	defer d.Close()
	entries, err := os.ReadDir(o.phrases)
	if err != nil {
		return "", err
	}
	silence := make([]int16, local.SampleRate)
	total, hits := 0, 0
	verdict := "PASS"
	cells := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		clips, err := filepath.Glob(filepath.Join(o.phrases, entry.Name(), "*.wav"))
		if err != nil {
			return "", err
		}
		sort.Strings(clips)
		cells++
		cellHits := 0
		for _, clip := range clips {
			pcm, err := local.ReadWAV(clip)
			if err != nil {
				return "", err
			}
			d.Reset()
			heard := false
			feed(d, slices.Concat(silence, pcm, silence), func(int) { heard = true })
			if heard {
				cellHits++
			}
		}
		total += len(clips)
		hits += cellHits
		_, _ = fmt.Fprintf(out, "cell=%s clips=%d heard=%d\n", entry.Name(), len(clips), cellHits)
		if len(clips) < o.minCell {
			_, _ = fmt.Fprintf(out, "UNKNOWN: cell %s holds %d clips, fewer than %d\n", entry.Name(), len(clips), o.minCell)
			verdict = "UNKNOWN"
		}
	}
	if cells == 0 || total == 0 {
		_, _ = fmt.Fprintf(out, "UNKNOWN: no cell of clips under %s\n", o.phrases)
		return "UNKNOWN", nil
	}
	rate := float64(hits) / float64(total)
	_, _ = fmt.Fprintf(out, "phrases=%d heard=%d hit_rate=%.3f\n", total, hits, rate)
	if verdict == "PASS" && rate < o.minHitRate {
		return "FAIL", nil
	}
	return verdict, nil
}
