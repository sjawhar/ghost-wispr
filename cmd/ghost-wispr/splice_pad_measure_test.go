package main

// TestMeasureDeletionAudioPad is the #65 review's leftover-audio probe (B1),
// run over several pads. It needs DEEPGRAM_API_KEY and PROBE_RECORDINGS (a
// comma-separated list of recordings), so it is skipped in CI. For 30 sampled
// refined segments of each recording it cuts a copy with each pad (production
// refinedSpliceBoundsWithPad and audio.Splice), transcribes the cut copy with
// the production Deepgram client, and counts a cut when a word of the deleted
// segment is heard near it beyond its neighbours' words (leftoverWords): within
// 2.5 s of the cut, the review's measure, and within deletionCutWindow, the
// check's. Middle-only cuts are its control. With PROBE_FIXTURE_DIR set it
// writes what it heard there, for TestDeletionCutCheck's fixtures.

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

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// reviewWindow is how near the cut the #65 review's probe counted a word.
const reviewWindow = 2.5

var measuredPads = []float64{0, 0.15, 0.25, 0.35, 0.45, 0.6, 0.8}

// deletionCutFixture is what the measurement heard around the cuts of one
// sampled segment: its words and its neighbours' within 6 s, and for each
// pad the cut and the cut recording's words within 3 s of it.
type deletionCutFixture struct {
	Recording string                 `json:"recording"`
	Segment   transcribe.Segment     `json:"segment"`
	Words     []storage.RefinedWord  `json:"words"`
	Cuts      []deletionCutHeardWith `json:"cuts"`
}

// controlCutPad marks a fixture's middle-only control cut.
const controlCutPad = -1

type deletionCutHeardWith struct {
	Pad   float64                      `json:"pad"`
	Start float64                      `json:"start"`
	End   float64                      `json:"end"`
	Heard []transcribe.TimestampedWord `json:"heard"`
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
		type job struct {
			seg     int
			pad     float64
			control bool
		}
		var jobs []job
		for _, i := range sample {
			for _, pad := range measuredPads {
				jobs = append(jobs, job{seg: i, pad: pad})
			}
		}
		for _, i := range sample[:5] {
			jobs = append(jobs, job{seg: i, control: true})
		}
		type result struct {
			start, end      float64
			review, check   []string
			heard           []transcribe.TimestampedWord
			neighbourSecond float64
		}
		results := make([]result, len(jobs))
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
					r := result{start: start, end: end}
					for k, w := range base {
						if k < seg.FirstWordOrdinal || k > seg.LastWordOrdinal {
							r.neighbourSecond += max(0, min(w.End, end)-max(w.Start, start))
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
					r.review = leftoverWords(base, &selected, start, end, cr.Words, reviewWindow)
					r.check = leftoverWords(base, &selected, start, end, cr.Words, deletionCutWindow)
					for _, w := range cr.Words {
						if w.End >= start-3 && w.Start <= start+3 {
							r.heard = append(r.heard, w)
						}
					}
					if len(r.review) > 0 {
						t.Logf("%s pad %.2f control=%t seg %d %q: heard %q within %.1f s, %q within %.1f s of the cut [%.3f,%.3f]",
							filepath.Base(rec), jb.pad, jb.control, jb.seg, storage.RefinedTranscript(base[seg.FirstWordOrdinal:seg.LastWordOrdinal+1]),
							r.review, reviewWindow, r.check, deletionCutWindow, start, end)
					}
					results[j] = r
				}
			})
		}
		for j := range jobs {
			work <- j
		}
		close(work)
		wg.Wait()

		name := filepath.Base(rec)
		for _, pad := range measuredPads {
			review, check, total, most := 0, 0, 0.0, 0.0
			for j, jb := range jobs {
				if jb.control || jb.pad != pad {
					continue
				}
				if len(results[j].review) > 0 {
					review++
				}
				if len(results[j].check) > 0 {
					check++
				}
				total += results[j].neighbourSecond
				most = max(most, results[j].neighbourSecond)
			}
			t.Logf("%s pad %.2f: a deleted word heard in %d of %d cuts within %.1f s, %d within %.1f s; neighbouring words' audio cut: mean %.3f s, max %.3f s",
				name, pad, review, len(sample), reviewWindow, check, deletionCutWindow, total/float64(len(sample)), most)
		}
		controlsReview, controlsCheck := 0, 0
		for j, jb := range jobs {
			if jb.control {
				if len(results[j].review) > 0 {
					controlsReview++
				}
				if len(results[j].check) > 0 {
					controlsCheck++
				}
			}
		}
		t.Logf("%s controls (middle-only cuts): %d of 5 heard within %.1f s, %d within %.1f s", name, controlsReview, reviewWindow, controlsCheck, deletionCutWindow)
		if controlsCheck == 0 {
			t.Errorf("%s: no middle-only control cut was heard within %.1f s; the measurement cannot fail", name, deletionCutWindow)
		}
		dir := os.Getenv("PROBE_FIXTURE_DIR")
		if dir == "" {
			continue
		}
		var fixtures []deletionCutFixture
		for _, i := range sample {
			seg := segs[i]
			fixture := deletionCutFixture{Recording: name, Segment: transcribe.Segment{StartTime: seg.Start, EndTime: seg.End, RecordingPath: "rec", RecordingSHA256: "sha"}}
			for _, w := range base {
				if w.End >= seg.Start-6 && w.Start <= seg.End+6 {
					fixture.Words = append(fixture.Words, w)
				}
			}
			for j, jb := range jobs {
				if jb.seg != i {
					continue
				}
				pad := jb.pad
				if jb.control {
					pad = controlCutPad
				}
				fixture.Cuts = append(fixture.Cuts, deletionCutHeardWith{Pad: pad, Start: results[j].start, End: results[j].end, Heard: results[j].heard})
			}
			fixtures = append(fixtures, fixture)
		}
		data, err := encodeDeletionCutFixtures(fixtures)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, strings.TrimSuffix(name, filepath.Ext(name))+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureHeardWindow is how far either side of a cut point a fixture keeps
// the words its transcription heard: more than any window the check uses.
const fixtureHeardWindow = 1.5

// encodeDeletionCutFixtures writes fixtures as testdata/deletion_cut holds
// them: each word with only its recording, text and times, and each cut's
// heard words with only their text and times, within fixtureHeardWindow of
// the cut point.
func encodeDeletionCutFixtures(fixtures []deletionCutFixture) ([]byte, error) {
	type word struct {
		RecordingPath, RecordingSHA256, Text string
		Start, End                           float64
	}
	type heard struct {
		Text       string
		Start, End float64
	}
	type cut struct {
		Pad   float64 `json:"pad"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Heard []heard `json:"heard"`
	}
	type fixture struct {
		Recording string             `json:"recording"`
		Segment   transcribe.Segment `json:"segment"`
		Words     []word             `json:"words"`
		Cuts      []cut              `json:"cuts"`
	}
	out := make([]fixture, 0, len(fixtures))
	for i := range fixtures {
		f := &fixtures[i]
		written := fixture{Recording: f.Recording, Segment: f.Segment, Words: []word{}, Cuts: []cut{}}
		for _, w := range f.Words {
			written.Words = append(written.Words, word{RecordingPath: w.RecordingPath, RecordingSHA256: w.RecordingSHA256, Text: w.Text, Start: w.Start, End: w.End})
		}
		for _, c := range f.Cuts {
			kept := cut{Pad: c.Pad, Start: c.Start, End: c.End, Heard: []heard{}}
			for _, h := range c.Heard {
				if h.End >= c.Start-fixtureHeardWindow && h.Start <= c.Start+fixtureHeardWindow {
					kept.Heard = append(kept.Heard, heard{Text: h.Text, Start: h.Start, End: h.End})
				}
			}
			written.Cuts = append(written.Cuts, kept)
		}
		out = append(out, written)
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
