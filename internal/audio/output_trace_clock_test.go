package audio

import (
	"os"
	"runtime/trace"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/speakertrace"
)

// TestTheReaderGivesASlowTurnsWallClockTime fails if the wall-clock end the
// reader gives a slow turn of the mixer, from a capture the real save path
// writes after the flight recorder has run 6.5 s (so older generations are
// gone and the capture starts seconds after the recorder did), is more than
// 10 ms from when the turn ended. A time counted from the capture's first
// event, added to the recorder's start, was about 3 s early in review.
func TestTheReaderGivesASlowTurnsWallClockTime(t *testing.T) {
	st, err := StartSpeakerTrace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Stop()
	time.Sleep(6500 * time.Millisecond)

	write := func(stall time.Duration) {
		r := trace.StartRegion(traceRegions, regionWrite)
		time.Sleep(stall)
		r.End()
	}
	write(5 * time.Millisecond)
	write(150 * time.Millisecond)
	ended := time.Now()
	write(5 * time.Millisecond)

	path, err := st.save(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cycles, err := speakertrace.Read(f, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var turn *speakertrace.Cycle
	for i := range cycles {
		if cycles[i].Length() >= 140*time.Millisecond && (turn == nil || cycles[i].Length() > turn.Length()) {
			turn = &cycles[i]
		}
	}
	if turn == nil {
		t.Fatalf("control: no turn of 140 ms or more in the capture, %d slow ones", len(cycles))
	}
	if turn.EndWall.IsZero() {
		t.Fatalf("the reader gave the slow turn no wall-clock time (%v into the capture)", turn.End)
	}
	if off := turn.EndWall.Sub(ended); off < -10*time.Millisecond || off > 10*time.Millisecond {
		t.Fatalf("the reader gave the turn's end as %s, %v from when it ended (%s), want within 10ms; %v into the capture",
			turn.EndWall.UTC().Format(time.RFC3339Nano), off, ended.UTC().Format(time.RFC3339Nano), turn.End)
	}
}
