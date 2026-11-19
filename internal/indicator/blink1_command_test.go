//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeBlink1Sysfs makes a sysfs root and a dev root that list one blink(1),
// hidraw0, and returns them with its node.
func fakeBlink1Sysfs(t *testing.T) (sysfsRoot, devRoot, node string) {
	t.Helper()
	sysfsRoot, devRoot = t.TempDir(), t.TempDir()
	device := filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device")
	if err := os.MkdirAll(device, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "uevent"), []byte("HID_ID=0003:000027B8:000001ED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	node = filepath.Join(devRoot, "hidraw0")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return sysfsRoot, devRoot, node
}

// TestLightOffReachesALightAnotherOpenHolds fails if `ghost-wispr light-off`
// sends nothing while another open of the node holds the lock. Its open's
// version query could not get the light, so the open failed before the off
// write, a lone SET that goes without the lock once its wait runs out (review
// of #79). The version waits until a command needs it.
func TestLightOffReachesALightAnotherOpenHolds(t *testing.T) {
	sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
	var (
		mu   sync.Mutex
		sets [][8]byte
	)
	original, originalWait := featureIOCTL, commandWait
	t.Cleanup(func() { featureIOCTL, commandWait = original, originalWait })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		mu.Lock()
		defer mu.Unlock()
		if request == hidIOCSetFeature {
			sets = append(sets, *report)
		} else if report[1] == 'v' {
			report[3] = '3'
		}
		return nil
	}
	commandWait = 100 * time.Millisecond

	holder, err := os.Open(node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
	if err != nil {
		t.Fatalf("light-off's open of a blink(1) another open holds: %v, want the light", err)
	}
	t.Cleanup(func() { _ = light.Close() })
	if err := light.SetRGB(0, 0, 0); err != nil {
		t.Fatalf("light-off's off write to a blink(1) another open holds: %v", err)
	}
	mu.Lock()
	got := append([][8]byte(nil), sets...)
	mu.Unlock()
	if want := Blink1Report('n', [6]byte{}); len(got) != 1 || got[0] != want {
		t.Fatalf("reports sent while another open held the lock = %v, want one off write %v", got, want)
	}

	// Control: once the other open lets go, a command that needs the version
	// reads it.
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := light.ServerDown(3 * time.Second); err != nil {
		t.Fatalf("watchdog tickle once the light is free: %v", err)
	}
	if lines := light.(*blink1).patternLines; lines != 32 {
		t.Fatalf("pattern lines after the version was read = %d, want 32", lines)
	}
}

// TestACommandOpenDoesNotFailForAnotherCommand fails if a subcommand's open
// (`light-state`, `light-off`) fails because another command had the light at
// its version query: the lock was held past its wait, or another command's
// reply answered the query. On a uhid blink(1), light-state printed nothing
// for a version query that the recorder's tickle answered (review of #79).
// The open must return the light, and a read once the light is free reads it.
func TestACommandOpenDoesNotFailForAnotherCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hold  bool // another open holds the lock at the open
		other bool // another command's reply answers the version query
	}{
		{name: "the lock held past the wait", hold: true},
		{name: "the version query answered by another command", other: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfsRoot, devRoot, node := fakeBlink1Sysfs(t)
			var answeredByOther atomic.Bool
			answeredByOther.Store(tc.other)
			original, originalWait := featureIOCTL, commandWait
			t.Cleanup(func() { featureIOCTL, commandWait = original, originalWait })
			commandWait = 50 * time.Millisecond
			featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
				if request != hidIOCGetFeature {
					return nil
				}
				switch report[1] {
				case 'v':
					if answeredByOther.Load() {
						*report = Blink1Report('D', [6]byte{1, 1, 44})
					} else {
						report[3] = '3'
					}
				case 'r':
					report[2] = 255
				}
				return nil
			}
			holder, err := os.Open(node)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Close() })
			if tc.hold {
				if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
					t.Fatal(err)
				}
			}

			light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
			if err != nil {
				t.Fatalf("a subcommand's open with %s: %v, want the light", tc.name, err)
			}
			t.Cleanup(func() { _ = light.Close() })

			answeredByOther.Store(false)
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if r, g, b, err := light.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
				t.Fatalf("control: read once the light is free = %d,%d,%d, %v, want 255,0,0", r, g, b, err)
			}
		})
	}
}

// TestAWaitingRecorderGoesBeforeLightState fails if the recorder's reads do
// not get the light within their wait while light-state runs back to back in
// four other processes. Such a read is unknown, and the gate drops that buffer
// of audio. The lock serves no queue, and the recorder polled for it as often
// as light-state did, so it lost the light to them: with 4 light-state loops
// on a uhid blink(1), 18 buffers were dropped in 20 s (review of #79). A
// waiting recorder must go first; light-state waits longer, and can.
//
// The recorder goes first only because the runtime wakes it every
// recorderPoll and light-state only every commandPoll. On a loaded host the
// runtime wakes sleeps late, and once a sleep of recorderPoll wakes later than
// commandPoll, every waiter wakes at once and that order is gone: with the
// host's load average at 40 to 60, 10 of 300 runs lost one read of about 70.
// So a probe sleeps recorderPoll over and over, and a read during which the
// probe woke later than commandPoll is not counted. The test reads until 40
// reads are counted, and fails if the runtime woke late through too many reads
// to count 40 in 10 s: it then measured nothing. A read is counted only once
// the probe has woken after it ends, since a late sleep is recorded when the
// probe wakes from it: counted earlier, a read that a late sleep then turned
// out to cover ended the loop short of 40 (with the load average at 400 to
// 2,000 on sami-devbox, 1 of 50 runs).
func TestAWaitingRecorderGoesBeforeLightState(t *testing.T) {
	sysfsRoot, devRoot, _ := fakeBlink1Sysfs(t)
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		time.Sleep(2 * time.Millisecond) // a USB control transfer: the sender holds the lock meanwhile
		if request == hidIOCGetFeature {
			switch report[1] {
			case 'v':
				report[3] = '3'
			case 'r':
				report[2] = 255
			}
		}
		return nil
	}
	recorder, err := OpenBlink1(sysfsRoot, devRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })

	stop := make(chan struct{})
	var loops sync.WaitGroup
	var runs atomic.Int32
	for range 4 {
		loops.Add(1)
		go func() {
			defer loops.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				light, err := OpenBlink1ForCommand(sysfsRoot, devRoot)
				if err != nil {
					continue
				}
				if _, _, _, err := light.ReadRGB(); err == nil {
					runs.Add(1)
				}
				_ = light.Close()
			}
		}()
	}
	late := watchLateTimers(stop, &loops)

	const counted = 40
	type read struct {
		from, to  time.Time
		contended bool
	}
	var done []read
	// cleanSettled counts the reads in done that the probe has woken after
	// and found no late sleep during.
	cleanSettled := func() int {
		clean := 0
		for _, r := range done {
			if late.settled(r.to) && !late.during(r.from, r.to) {
				clean++
			}
		}
		return clean
	}
	start := time.Now()
	for deadline := start.Add(10 * time.Second); cleanSettled() < counted && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		from := time.Now()
		_, _, _, err := recorder.ReadRGB()
		r := read{from: from, to: time.Now(), contended: Contended(err)}
		if err != nil && !r.contended {
			t.Fatalf("the recorder's read: %v", err)
		}
		done = append(done, r)
	}
	took := time.Since(start)
	close(stop)
	loops.Wait()
	clean, unknown, excused := 0, 0, 0
	for _, r := range done {
		lateDuring := late.during(r.from, r.to)
		if !lateDuring {
			clean++
		}
		switch {
		case r.contended && lateDuring:
			excused++
		case r.contended:
			unknown++
		}
	}
	if n := runs.Load(); n < 20 {
		t.Fatalf("control: light-state read the light %d times in %v, want at least 20", n, took.Round(time.Millisecond))
	}
	if clean < counted {
		t.Fatalf("control: the runtime woke a %v sleep later than %v during %d of %d reads in %v, so only %d could be counted, want %d: this run measured nothing", recorderPoll, commandPoll, len(done)-clean, len(done), took.Round(time.Millisecond), clean, counted)
	}
	if unknown > 0 {
		t.Fatalf("the recorder's read got no light %d of %d times while light-state ran back to back in 4 processes, want 0 (%d more reads got no light while the runtime woke late)", unknown, clean, excused)
	}
	t.Logf("the recorder read the light %d times, %d counted; %d reads that the runtime woke late through got no light; light-state read it %d times", len(done), clean, excused, runs.Load())
}

// lateTimers is the stretches in which the runtime woke a sleep of
// recorderPoll later than commandPoll (watchLateTimers), and when the probe
// last woke.
type lateTimers struct {
	mu    sync.Mutex
	spans [][2]time.Time
	woke  time.Time
}

// watchLateTimers sleeps recorderPoll over and over until stop closes, and
// records each sleep that woke later than commandPoll. loops counts its
// goroutine.
func watchLateTimers(stop <-chan struct{}, loops *sync.WaitGroup) *lateTimers {
	late := &lateTimers{}
	loops.Add(1)
	go func() {
		defer loops.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			from := time.Now()
			time.Sleep(recorderPoll)
			to := time.Now()
			late.mu.Lock()
			if to.Sub(from) > commandPoll {
				late.spans = append(late.spans, [2]time.Time{from, to})
			}
			late.woke = to
			late.mu.Unlock()
		}
	}()
	return late
}

// settled reports whether the probe has woken since to: every late sleep
// that could overlap a stretch ending at to is recorded by then, as the
// probe's next sleep starts after it wakes.
func (l *lateTimers) settled(to time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.woke.After(to)
}

// during reports whether a late sleep overlapped the stretch from from to to.
// A sleep still under way is not seen until the probe wakes from it.
func (l *lateTimers) during(from, to time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, span := range l.spans {
		if span[0].Before(to) && span[1].After(from) {
			return true
		}
	}
	return false
}
