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
// streams once every -splice-every of audio: the first control, which shows
// the listener can hear the phrase in that speech, so that 0 false starts is
// not a deaf listener's. A wake within 2 s of a spliced clip's end counts as a
// spliced hit, not a false start, and the spliced audio is not counted in the
// hours. With no -splice, the clips of -phrases are spliced in.
//
// The second control runs the same pipeline and counter on the same speech,
// unspliced, with a keyword the speech says often, -control-keyword ("AND
// THEN"), in place of the start phrase (check c-listener-accuracy). It must
// count at least -control-min hits: else the counter is not shown to see
// spontaneous hits in that speech, and 0 false starts proves nothing.
//
// -phrases is a directory whose subdirectories are cells (a person at a
// distance), each holding WAV files of one start phrase each; each clip is
// played between seconds of silence, and is a hit if the listener accepts it.
//
// The run prints key=value lines, then a verdict: FAIL when it read at least
// -min-hours with at least -limit false starts per hour, or with -phrases
// heard fewer than -min-hit-rate of them; PASS when it read at least
// -min-hours with fewer than -limit false starts per hour, both controls
// fired, and with -phrases every cell holds at least -min-cell clips and it
// heard at least -min-hit-rate of them; UNKNOWN otherwise: too little audio,
// a small cell, or a control that did not fire.
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
	mode           string
	audio          string
	phrases        string
	splice         string
	spliceEvery    time.Duration
	boost          float64
	threshold      float64
	decoys         string
	stage2         bool
	controlKeyword string
	controlMin     int
	// keyword, when set, is what the detector listens for in place of the
	// start phrase, with stage 2 checking it: the control's (keywordLine).
	keyword    string
	parallel   int
	minHours   float64
	limit      float64
	minHitRate float64
	minCell    int
	each       bool
	// detector makes each detector the run uses: newDetector, the
	// listener's own.
	detector func(*options) (*local.Detector, error)
}

func run(args []string, stdout, stderr io.Writer, detector func(*options) (*local.Detector, error)) int {
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
	flags.StringVar(&o.controlKeyword, "control-keyword", "AND THEN", "the control's keyword, common in -audio: words that are each one of the model's tokens")
	flags.IntVar(&o.controlMin, "control-min", 10, "the fewest hits the control must count in -audio")
	flags.IntVar(&o.parallel, "parallel", 1, "detectors that share -audio's streams, each on its own CPU")
	flags.Float64Var(&o.minHours, "min-hours", 10, "the least audio a verdict rests on")
	flags.Float64Var(&o.limit, "limit", 0.5, "false starts per hour the verdict allows, exclusive")
	flags.Float64Var(&o.minHitRate, "min-hit-rate", 0.9, "the share of -phrases the verdict needs")
	flags.IntVar(&o.minCell, "min-cell", 10, "the fewest clips a cell of -phrases may hold")
	flags.BoolVar(&o.each, "each", false, "with -phrases, print each clip: whether stage 1 fired and whether the listener heard it")
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
	verdict, err := measure(&o, stdout)
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

func newDetector(o *options) (*local.Detector, error) {
	opts := local.Options{Thresholds: local.Thresholds{Boost: o.boost, Threshold: o.threshold}, NoStageTwo: !o.stage2}
	if o.keyword != "" {
		opts.Keywords, opts.Mark = keywordLine(o.keyword, o.boost, o.threshold), o.keyword
	}
	switch o.decoys {
	case "":
	case "stop":
		opts.Decoys = local.StopDecoys
	default:
		return nil, fmt.Errorf("-decoys %q: only stop", o.decoys)
	}
	return local.NewDetectorWith(&opts)
}

// keywordLine is the keyword spotter's line for words, each one of the
// model's word tokens ("▁AND"), at boost and threshold. The detector refuses a
// word the model has no token for.
func keywordLine(words string, boost, threshold float64) string {
	fields := strings.Fields(strings.ToUpper(words))
	for i, w := range fields {
		fields[i] = "▁" + w
	}
	return fmt.Sprintf("%s :%g #%g @CONTROL\n", strings.Join(fields, " "), boost, threshold)
}

// measure runs the listener's detector over the audio and the phrases and
// returns the verdict.
func measure(o *options, out io.Writer) (string, error) {
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

// countAll runs a detector of o's on each of -parallel shares of the streams
// at once, and sums what they counted.
func countAll(o *options, all [][]string, spliced [][]int16, spliceEvery int) (speechCounts, error) {
	detectors := make([]*local.Detector, o.parallel)
	defer func() {
		for _, d := range detectors {
			if d != nil {
				d.Close()
			}
		}
	}()
	for i := range detectors {
		d, err := o.detector(o)
		if err != nil {
			return speechCounts{}, err
		}
		detectors[i] = d
	}
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
	return total, first
}

// spliceClips reads the clips the run splices into the speech: -splice's, else
// every clip of -phrases.
func spliceClips(o *options) ([][]int16, error) {
	var clips []string
	var err error
	switch {
	case o.splice != "":
		clips, err = filepath.Glob(filepath.Join(o.splice, "*.wav"))
	case o.phrases != "":
		clips, err = filepath.Glob(filepath.Join(o.phrases, "*", "*.wav"))
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(clips)
	var spliced [][]int16
	for _, clip := range clips {
		pcm, err := local.ReadWAV(clip)
		if err != nil {
			return nil, err
		}
		spliced = append(spliced, pcm)
	}
	if o.splice != "" && len(spliced) == 0 {
		return nil, fmt.Errorf("-splice %s holds no WAV clip", o.splice)
	}
	return spliced, nil
}

func measureFalseStarts(o *options, out io.Writer) (string, error) {
	all, err := streams(o.audio)
	if err != nil {
		return "", err
	}
	spliced, err := spliceClips(o)
	if err != nil {
		return "", err
	}
	spliceEvery := int(o.spliceEvery.Seconds() * local.SampleRate)
	started := cpuTime()
	total, err := countAll(o, all, spliced, spliceEvery)
	if err != nil {
		return "", err
	}
	used := cpuTime() - started
	// The control runs after the listener, so the CPU above is the listener's.
	// It is the same pipeline, listening for the common keyword instead.
	control := *o
	control.keyword, control.decoys = strings.Join(strings.Fields(strings.ToUpper(o.controlKeyword)), " "), ""
	controlCounts, err := countAll(&control, all, nil, 0)
	if err != nil {
		return "", err
	}

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
	_, _ = fmt.Fprintf(out, "spliced_clips=%d spliced_heard=%d\n", total.splicedClips, total.splicedHeard)
	controlPerHour := 0.0
	if hours > 0 {
		controlPerHour = float64(controlCounts.falseStarts) / hours
	}
	_, _ = fmt.Fprintf(out, "control_keyword=%q control_hits=%d control_hits_per_hour=%.3f\n", control.keyword, controlCounts.falseStarts, controlPerHour)
	switch {
	case total.files == 0:
		_, _ = fmt.Fprintf(out, "UNKNOWN: no WAV file read under %s\n", o.audio)
		return "UNKNOWN", nil
	case hours < o.minHours:
		_, _ = fmt.Fprintf(out, "UNKNOWN: %.3f h of audio, fewer than %g h\n", hours, o.minHours)
		return "UNKNOWN", nil
	case perHour >= o.limit:
		return "FAIL", nil
	case total.splicedClips == 0:
		_, _ = fmt.Fprintln(out, "UNKNOWN: no start phrase was spliced into the speech (give -splice or -phrases, and -splice-every under the length of a stream), so nothing shows the listener can hear one")
		return "UNKNOWN", nil
	case total.splicedHeard == 0:
		_, _ = fmt.Fprintln(out, "UNKNOWN: the listener heard none of the spliced phrases, so a count of 0 false starts proves nothing")
		return "UNKNOWN", nil
	case controlCounts.falseStarts < o.controlMin || controlCounts.falseStarts == 0:
		_, _ = fmt.Fprintf(out, "UNKNOWN: the common-keyword control heard %q %d times, fewer than %d: the counter is not shown to see spontaneous hits in this speech\n", control.keyword, controlCounts.falseStarts, max(o.controlMin, 1))
		return "UNKNOWN", nil
	default:
		return "PASS", nil
	}
}

func measurePhrases(o *options, out io.Writer) (string, error) {
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
			heard, before := false, d.StageOneHits()
			feed(d, slices.Concat(silence, pcm, silence), func(int) { heard = true })
			if heard {
				cellHits++
			}
			if o.each {
				_, _ = fmt.Fprintf(out, "clip=%s/%s stage1=%d heard=%t\n", entry.Name(), filepath.Base(clip), d.StageOneHits()-before, heard)
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
