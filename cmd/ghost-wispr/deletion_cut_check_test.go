package main

// The check after each "Delete text and audio" cut, run on what Deepgram heard
// in real cuts. testdata/deletion_cut holds TestMeasureDeletionAudioPad's
// recordings of the #65 review's two recordings (a public-domain NASA podcast,
// clean and with room echo and noise): for 30 segments of each, the refined
// words around them, and for each pad, and for a middle-only control cut, the
// words a batch transcription of the cut copy heard around the cut. A fake
// transcription replays them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func loadDeletionCutFixtures(t *testing.T) []deletionCutFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "deletion_cut", "*.json"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("deletion cut fixtures = %v (%v), want the two recordings'", paths, err)
	}
	var all []deletionCutFixture
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixtures []deletionCutFixture
		if err := json.Unmarshal(data, &fixtures); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(fixtures) != 30 {
			t.Fatalf("%s holds %d segments, want 30", path, len(fixtures))
		}
		all = append(all, fixtures...)
	}
	return all
}

// TestDeletionCutFixturesAreWhatTheMeasurementWrites fails if a checked-in
// fixture differs from what TestMeasureDeletionAudioPad writes for its data,
// such as one trimmed by hand.
func TestDeletionCutFixturesAreWhatTheMeasurementWrites(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "deletion_cut", "*.json"))
	if err != nil || len(paths) != 2 {
		t.Fatalf("deletion cut fixtures = %v (%v), want the two recordings'", paths, err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixtures []deletionCutFixture
		if err := json.Unmarshal(data, &fixtures); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		written, err := encodeDeletionCutFixtures(fixtures)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(written, data) {
			t.Errorf("%s (%d bytes) is not what the measurement writes for its data (%d bytes)", path, len(data), len(written))
		}
	}
}

// heardAt reports whether the fixture's cut with pad leaves a word of its
// segment heard next to the cut, by the check's rule.
func heardAt(f *deletionCutFixture, pad float64) bool {
	for _, cut := range f.Cuts {
		if cut.Pad == pad {
			return len(leftoverWords(f.Words, &f.Segment, cut.Start, cut.End, cut.Heard, deletionCutWindow)) > 0
		}
	}
	return false
}

type replayedCut struct {
	tried    []float64
	accepted deletionCutHeardWith
	err      error
	leftover []string // files left beside the recording
}

// replayCut runs the production check on fixture f: the splice only records
// its bounds, and the transcription of the cut copy replays what the
// measurement heard for the cut with those bounds.
func replayCut(t *testing.T, f *deletionCutFixture) replayedCut {
	t.Helper()
	dir := t.TempDir()
	recording := filepath.Join(dir, "recording.mp3")
	if err := os.WriteFile(recording, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	bounds := map[string][2]float64{}
	var result replayedCut
	runner := &segmentDeleteRunner{
		splice: func(_ context.Context, path string, start, end float64) (string, error) {
			bounds[path] = [2]float64{start, end}
			return "cut-hash", nil
		},
		transcribeCut: func(_ context.Context, model, path string) ([]transcribe.TimestampedWord, error) {
			if model != "nova-3" {
				t.Errorf("cut transcribed with model %q, want the session's nova-3", model)
			}
			b := bounds[path]
			for _, cut := range f.Cuts {
				if cut.Pad >= 0 && math.Abs(cut.Start-b[0]) < 1e-6 && math.Abs(cut.End-b[1]) < 1e-6 {
					result.tried = append(result.tried, cut.Pad)
					return cut.Heard, nil
				}
			}
			t.Fatalf("%s segment at %.3f s: no recorded cut at [%.3f, %.3f]", f.Recording, f.Segment.StartTime, b[0], b[1])
			return nil, nil
		},
	}
	path, start, end, _, err := runner.cutAndCheck(context.Background(), "nova-3", recording, f.Words, &f.Segment, -1)
	result.err = err
	if err == nil {
		result.accepted = deletionCutHeardWith{Pad: result.tried[len(result.tried)-1], Start: start, End: end}
		_ = os.Remove(path)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() != "recording.mp3" {
			result.leftover = append(result.leftover, entry.Name())
		}
	}
	return result
}

// TestDeletionCutCheckHearsTheControls fails if the check's rule cannot hear a
// deleted word in the recorded middle-only cuts, which leave most of each
// segment: then it could pass every real cut without hearing anything.
func TestDeletionCutCheckHearsTheControls(t *testing.T) {
	controls, heard := 0, 0
	fixtures := loadDeletionCutFixtures(t)
	for i := range fixtures {
		if heardAt(&fixtures[i], controlCutPad) {
			heard++
		}
		for _, cut := range fixtures[i].Cuts {
			if cut.Pad == controlCutPad {
				controls++
			}
		}
	}
	if controls != 10 || heard < controls/2 {
		t.Fatalf("the check heard a deleted word in %d of %d middle-only control cuts, want most of 10", heard, controls)
	}
	t.Logf("the check heard a deleted word in %d of %d middle-only control cuts", heard, controls)
}

// TestDeletionCutFixturesBackThePadComment fails if the recorded cuts no
// longer show what deletionAudioPad's comment says of them: by the check's
// rule, how many cuts of each recording left a deleted word heard with no pad
// and with the pad, and how much neighbouring speech the pad cuts.
func TestDeletionCutFixturesBackThePadComment(t *testing.T) {
	type measure struct {
		heard             int
		meanCost, maxCost float64
	}
	byRecording := map[string]map[float64]*measure{}
	fixtures := loadDeletionCutFixtures(t)
	for i := range fixtures {
		f := &fixtures[i]
		if byRecording[f.Recording] == nil {
			byRecording[f.Recording] = map[float64]*measure{0: {}, deletionAudioPad: {}}
		}
		for _, cut := range f.Cuts {
			m := byRecording[f.Recording][cut.Pad]
			if m == nil {
				continue
			}
			if heardAt(f, cut.Pad) {
				m.heard++
			}
			cost := 0.0
			for _, word := range f.Words {
				if word.Start >= f.Segment.StartTime && word.End <= f.Segment.EndTime {
					continue
				}
				cost += max(0, min(word.End, cut.End)-max(word.Start, cut.Start))
			}
			m.meanCost += cost / 30
			m.maxCost = max(m.maxCost, cost)
		}
	}
	for recording, want := range map[string]struct {
		noPad, pad int
		meanCost   float64
	}{"clean.mp3": {9, 1, 0.66}, "room.mp3": {6, 2, 0.72}} {
		got := byRecording[recording]
		if got[0].heard != want.noPad || got[deletionAudioPad].heard != want.pad {
			t.Errorf("%s: cuts with a deleted word heard = %d with no pad and %d with %.2f s, the comment says %d and %d",
				recording, got[0].heard, got[deletionAudioPad].heard, deletionAudioPad, want.noPad, want.pad)
		}
		if cost := got[deletionAudioPad]; math.Abs(cost.meanCost-want.meanCost) > 0.005 || cost.maxCost > 0.9+1e-9 {
			t.Errorf("%s: neighbouring speech cut at %.2f s = mean %.3f s, max %.3f s; the comment says %.2f s, at most 0.90 s",
				recording, deletionAudioPad, cost.meanCost, cost.maxCost, want.meanCost)
		}
	}
}

// TestDeletionCutCheckCatchesALeftoverAndWidens fails if a recorded cut that
// left a deleted word heard next to it is accepted, or if the check does not
// widen to the next pad and accept that cut when it is clean.
func TestDeletionCutCheckCatchesALeftoverAndWidens(t *testing.T) {
	fixtures := loadDeletionCutFixtures(t)
	cases := 0
	for i := range fixtures {
		f := &fixtures[i]
		if !heardAt(f, deletionCutPads[0]) || heardAt(f, deletionCutPads[1]) {
			continue
		}
		cases++
		got := replayCut(t, f)
		if got.err != nil {
			t.Errorf("%s segment at %.3f s: refused %v, want the %.2f s cut", f.Recording, f.Segment.StartTime, got.err, deletionCutPads[1])
			continue
		}
		if !slices.Equal(got.tried, deletionCutPads[:2]) || got.accepted.Pad != deletionCutPads[1] {
			t.Errorf("%s segment at %.3f s: tried pads %v and accepted %.2f s, want %v and %.2f s", f.Recording, f.Segment.StartTime, got.tried, got.accepted.Pad, deletionCutPads[:2], deletionCutPads[1])
		}
		if len(got.leftover) != 0 {
			t.Errorf("%s segment at %.3f s: left %v beside the recording", f.Recording, f.Segment.StartTime, got.leftover)
		}
	}
	if cases == 0 {
		t.Fatalf("no recorded segment had a leftover at %.2f s and none at %.2f s; the fixtures cannot show the check widening", deletionCutPads[0], deletionCutPads[1])
	}
	t.Logf("%d recorded segments widened from %.2f s to %.2f s", cases, deletionCutPads[0], deletionCutPads[1])
}

// TestDeletionCutCheckRefuses fails if a segment whose every recorded cut
// left a deleted word heard next to it is cut anyway, or if a refused cut
// leaves a copy of the recording behind. No recorded segment was heard at all
// three production pads (Deepgram heard the one that comes nearest clean at
// 0.45 s once), so it tries the last two: the recorded segment at 392.735 s
// was heard at 0.6 s and 0.8 s on both recordings.
func TestDeletionCutCheckRefuses(t *testing.T) {
	original := deletionCutPads
	deletionCutPads = original[1:]
	t.Cleanup(func() { deletionCutPads = original })
	fixtures := loadDeletionCutFixtures(t)
	cases := 0
	for i := range fixtures {
		f := &fixtures[i]
		if !heardAt(f, deletionCutPads[0]) || !heardAt(f, deletionCutPads[1]) {
			continue
		}
		cases++
		got := replayCut(t, f)
		if !errors.Is(got.err, errDeletionCutHeard) {
			t.Errorf("%s segment at %.3f s: result %v after pads %v, want refused", f.Recording, f.Segment.StartTime, got.err, got.tried)
		}
		if !slices.Equal(got.tried, deletionCutPads) || len(got.leftover) != 0 {
			t.Errorf("%s segment at %.3f s: tried %v, left %v beside the recording; want every pad tried and nothing left", f.Recording, f.Segment.StartTime, got.tried, got.leftover)
		}
	}
	if cases == 0 {
		t.Fatalf("no recorded segment had a leftover at every pad; the fixtures cannot show the check refusing")
	}
	t.Logf("%d recorded segments refused", cases)
}

// TestDeletionCutCheckAcceptsACleanCut fails if a recorded cut that left no
// deleted word heard is not accepted at once.
func TestDeletionCutCheckAcceptsACleanCut(t *testing.T) {
	fixtures := loadDeletionCutFixtures(t)
	for i := range fixtures {
		f := &fixtures[i]
		if heardAt(f, deletionCutPads[0]) {
			continue
		}
		got := replayCut(t, f)
		if got.err != nil || !slices.Equal(got.tried, deletionCutPads[:1]) {
			t.Fatalf("%s segment at %.3f s: result %v after pads %v, want the %.2f s cut at once", f.Recording, f.Segment.StartTime, got.err, got.tried, deletionCutPads[0])
		}
		return
	}
	t.Fatal("no recorded segment had a clean cut at the first pad")
}
