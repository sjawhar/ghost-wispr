package speakertrace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The traces in testdata/clock are Go 1.25 execution traces written event by
// event in review 6003678369 of #134, with a clock history the test knows.
// The trace clock counts nanoseconds. The mixer's writes end at 1.1, 1.25,
// 1.4 and 1.55 s of trace time, and also at 2.1 and 2.25 s in the step
// fixtures. The wall clock at 1 s of trace time is the time in the fixture's
// .origin file, and it steps by the fixture's step at 1.5 s, which no
// snapshot shows. The clock snapshots are at 1 s (and 2 s) of trace time, or
// at 1.8 s in the late ones; no-snapshot is a Go 1.23 trace, which has none.
type clockFixture struct {
	name string
	step time.Duration
	// late: the only snapshot was sampled at 1.8 s, after the first turns.
	late    bool
	none    bool
	twoGens bool
}

var clockFixtures = []clockFixture{
	{name: "steady"},
	{name: "step-forward", step: time.Minute, twoGens: true},
	{name: "step-backward", step: -time.Minute, twoGens: true},
	{name: "late-snapshot", late: true},
	{name: "late-snapshot-step", step: time.Minute, late: true, twoGens: true},
	{name: "no-snapshot", none: true},
}

const second = time.Second

// TestWallClockTimesAreQualifiedWhereTheSnapshotsCannotPinThem fails if the
// reader gives a plain wall-clock time that may be wrong: a time between two
// clock snapshots whose offsets show the clock stepped (60 s early or late
// before, review 6003678369 of #134), or a time before the first snapshot
// sampled, projected from a later one. Each must be qualified, and an
// interval must hold the true time. It also fails if a time on a steady
// clock, after a snapshot, is qualified or more than 1 µs off, or if a trace
// with no snapshot gets a wall-clock time.
func TestWallClockTimesAreQualifiedWhereTheSnapshotsCannotPinThem(t *testing.T) {
	for _, fx := range clockFixtures {
		t.Run(fx.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "clock", fx.name+".origin"))
			if err != nil {
				t.Fatal(err)
			}
			origin, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(filepath.Join("testdata", "clock", fx.name+".trace"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			cycles, err := Read(f, 40*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			ends := []time.Duration{1100 * time.Millisecond, 1250 * time.Millisecond, 1400 * time.Millisecond, 1550 * time.Millisecond}
			if fx.twoGens {
				ends = append(ends, 2100*time.Millisecond, 2250*time.Millisecond)
			}
			if len(cycles) != len(ends)-1 {
				t.Fatalf("control: %d slow turns, want %d, one between each two writes", len(cycles), len(ends)-1)
			}
			// The trace time the reader counts from: the first turn ends at
			// the second write.
			first := ends[1] - cycles[0].End
			check := func(what string, at time.Duration, w Wall) {
				t.Helper()
				trueAt := origin.Add(first + at - second)
				if first+at > 1500*time.Millisecond {
					trueAt = trueAt.Add(fx.step)
				}
				abs := first + at
				switch {
				case fx.none:
					if !w.IsZero() {
						t.Errorf("%s at %v: a trace with no clock snapshot got the wall-clock time %v", what, abs, w)
					}
				case fx.late && abs < 1800*time.Millisecond:
					if !strings.Contains(w.Note, "no clock snapshot before it") {
						t.Errorf("%s at %v: no snapshot before it, but the time %v is not qualified (%q)", what, abs, w.From, w.Note)
					}
					if fx.step == 0 && w.From.Sub(trueAt).Abs() > time.Microsecond {
						t.Errorf("%s at %v: projected back on a steady clock to %v, want %v", what, abs, w.From, trueAt)
					}
				case fx.step != 0 && abs > second && abs < 2*second:
					if !strings.Contains(w.Note, "stepped") || !w.From.Before(w.To) {
						t.Errorf("%s at %v: between snapshots that show a %v step, given as %v to %v (%q), want an interval, qualified", what, abs, fx.step, w.From, w.To, w.Note)
					}
					if trueAt.Before(w.From.Add(-time.Microsecond)) || trueAt.After(w.To.Add(time.Microsecond)) {
						t.Errorf("%s at %v: the true time %v is outside the interval %v to %v", what, abs, trueAt, w.From, w.To)
					}
				default:
					if w.Note != "" || !w.From.Equal(w.To) {
						t.Errorf("%s at %v: on a steady clock after a snapshot, qualified: %v to %v (%q)", what, abs, w.From, w.To, w.Note)
					}
					if w.From.Sub(trueAt).Abs() > time.Microsecond {
						t.Errorf("%s at %v: wall-clock time %v, want %v", what, abs, w.From, trueAt)
					}
				}
			}
			for i := range cycles {
				c := &cycles[i]
				check("start", c.Start, c.StartWall)
				check("end", c.End, c.EndWall)
			}
		})
	}
}
