package main

// TestMeasureDeletionAudioPad is the #65 review's leftover-audio probe (B1),
// run over several pads. It needs DEEPGRAM_API_KEY and PROBE_RECORDINGS (a
// comma-separated list of recordings), so it is skipped in CI; it writes the
// fixture TestDeletionCutCoversWhatWasHeard reads to PROBE_FIXTURE_DIR. For
// 30 sampled refined segments of each recording it cuts a copy with each pad
// (production refinedSpliceBoundsWithPad and audio.Splice), transcribes the
// cut copy with the production Deepgram client, and counts a cut when a word
// of the deleted segment is heard within 2.5 s of it and not among its
// neighbours' words. Pad 0 is the bounds before the pad.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

var measuredPads = []float64{0, 0.15, 0.25, 0.35, 0.45, 0.6}

// splicePadFixture is one recording's measurement: its refined words around
// each sampled segment, and for each segment the smallest measured pad whose
// cut left none of its words heard (-1 when none did).
type splicePadFixture struct {
	Recording string               `json:"recording"`
	Segments  []splicePadSegment   `json:"segments"`
	Pads      []float64            `json:"pads"`
	Heard     map[string][]int     `json:"heard"` // pad -> sampled segment indexes with a word heard
	Cost      map[string][]float64 `json:"neighbour_seconds"`
}

type splicePadSegment struct {
	Start, End float64
	Words      []storage.RefinedWord // the segment's and its neighbours' within 5 s
	CleanPad   float64
}

func normWord(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func wordCounts(words []storage.RefinedWord) map[string]int {
	m := map[string]int{}
	for _, w := range words {
		if n := normWord(w.Text); n != "" {
			m[n]++
		}
	}
	return m
}

func TestMeasureDeletionAudioPad(t *testing.T) {
	recordings, key := os.Getenv("PROBE_RECORDINGS"), os.Getenv("DEEPGRAM_API_KEY")
	if recordings == "" || key == "" {
		t.Skip("PROBE_RECORDINGS and DEEPGRAM_API_KEY are required")
	}
	ctx := context.Background()
	dg := transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{APIKey: key, Model: "nova-3"})
	for _, rec := range strings.Split(recordings, ",") {
		out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1", rec).Output()
		if err != nil {
			t.Fatal(err)
		}
		duration, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		res, err := dg.Transcribe(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		base := make([]storage.RefinedWord, len(res.Words))
		for i, w := range res.Words {
			base[i] = storage.RefinedWord{RecordingPath: "rec", RecordingSHA256: "sha", Ordinal: i, Text: w.Text, Speaker: w.Speaker, Start: w.Start, End: w.End}
		}
		segs := storage.BuildRefinedSegments(base)
		var sample []int
		step := max((len(segs)-6)/30, 1)
		for i := 3; i < len(segs)-3 && len(sample) < 30; i += step {
			sample = append(sample, i)
		}
		// Middle-only control cuts: the measurement must find their words.
		type job struct {
			seg     int
			pad     float64
			control bool
		}
		var jobs []job
		for _, pad := range measuredPads {
			for _, i := range sample {
				jobs = append(jobs, job{seg: i, pad: pad})
			}
		}
		for _, i := range sample[:5] {
			jobs = append(jobs, job{seg: i, control: true})
		}
		heard := make([]bool, len(jobs))
		cost := make([]float64, len(jobs))
		work := make(chan int)
		var wg sync.WaitGroup
		tmp := t.TempDir()
		for range 8 {
			wg.Go(func() {
				for j := range work {
					jb := jobs[j]
					seg := segs[jb.seg]
					selected := transcribe.Segment{StartTime: seg.Start, EndTime: seg.End, RecordingPath: "rec", RecordingSHA256: "sha"}
					start, end := refinedSpliceBoundsWithPad(base, &selected, jb.pad)
					end = min(end, duration)
					if jb.control {
						d := seg.End - seg.Start
						start, end = seg.Start+0.35*d, seg.End-0.35*d
					}
					var before, after []storage.RefinedWord
					for k, w := range base {
						inSeg := k >= seg.FirstWordOrdinal && k <= seg.LastWordOrdinal
						if !inSeg {
							cost[j] += max(0, min(w.End, end)-max(w.Start, start))
						}
						if w.End <= start+1e-6 && w.Start >= start-3.5 {
							before = append(before, w)
						}
						if w.Start >= end-1e-6 && w.Start < end+3.5 {
							after = append(after, w)
						}
					}
					cut := filepath.Join(tmp, fmt.Sprintf("cut-%d.mp3", j))
					data, err := os.ReadFile(rec)
					if err == nil {
						err = os.WriteFile(cut, data, 0o600)
					}
					if err == nil {
						_, err = audio.Splice(ctx, cut, start, end)
					}
					var cr transcribe.BatchResult
					if err == nil {
						cr, err = dg.Transcribe(ctx, cut)
					}
					_ = os.Remove(cut)
					if err != nil {
						t.Errorf("cut %d: %v", j, err)
						continue
					}
					var window []storage.RefinedWord
					for _, w := range cr.Words {
						if w.Start >= start-2.5 && w.Start < start+2.5 {
							window = append(window, storage.RefinedWord{Text: w.Text})
						}
					}
					near := wordCounts(before)
					for k, v := range wordCounts(after) {
						near[k] += v
					}
					segWords := wordCounts(base[seg.FirstWordOrdinal : seg.LastWordOrdinal+1])
					for k, v := range wordCounts(window) {
						if v > near[k] && segWords[k] > 0 {
							heard[j] = true
							t.Logf("%s pad %.2f control=%t seg %d %q: heard %q around the cut [%.3f,%.3f]", filepath.Base(rec), jb.pad, jb.control, jb.seg, storage.RefinedTranscript(base[seg.FirstWordOrdinal:seg.LastWordOrdinal+1]), k, start, end)
						}
					}
				}
			})
		}
		for j := range jobs {
			work <- j
		}
		close(work)
		wg.Wait()

		fixture := splicePadFixture{Recording: filepath.Base(rec), Pads: measuredPads, Heard: map[string][]int{}, Cost: map[string][]float64{}}
		clean := map[int]float64{}
		for _, i := range sample {
			clean[i] = -1
		}
		controlsHeard := 0
		for j, jb := range jobs {
			if jb.control {
				if heard[j] {
					controlsHeard++
				}
				continue
			}
			key := strconv.FormatFloat(jb.pad, 'f', 2, 64)
			if heard[j] {
				fixture.Heard[key] = append(fixture.Heard[key], jb.seg)
			} else if clean[jb.seg] < 0 || jb.pad < clean[jb.seg] {
				clean[jb.seg] = jb.pad
			}
			fixture.Cost[key] = append(fixture.Cost[key], cost[j])
		}
		for _, pad := range measuredPads {
			key := strconv.FormatFloat(pad, 'f', 2, 64)
			total, most := 0.0, 0.0
			for _, c := range fixture.Cost[key] {
				total += c
				most = max(most, c)
			}
			t.Logf("%s pad %s: %d of %d cuts with a deleted word heard; neighbouring words' audio cut: mean %.3f s, max %.3f s",
				fixture.Recording, key, len(fixture.Heard[key]), len(sample), total/float64(len(sample)), most)
		}
		t.Logf("%s controls: %d of 5 middle-only cuts heard", fixture.Recording, controlsHeard)
		if controlsHeard == 0 {
			t.Errorf("%s: no middle-only control cut was heard; the measurement cannot fail", fixture.Recording)
		}
		for _, i := range sample {
			seg := segs[i]
			var words []storage.RefinedWord
			for _, w := range base {
				if w.End >= seg.Start-5 && w.Start <= seg.End+5 {
					words = append(words, w)
				}
			}
			fixture.Segments = append(fixture.Segments, splicePadSegment{Start: seg.Start, End: seg.End, Words: words, CleanPad: clean[i]})
		}
		if dir := os.Getenv("PROBE_FIXTURE_DIR"); dir != "" {
			data, err := json.MarshalIndent(fixture, "", " ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, strings.TrimSuffix(fixture.Recording, filepath.Ext(fixture.Recording))+".json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
