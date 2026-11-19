//go:build linux && (amd64 || arm64)

package indicator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAStoppedHolderDoesNotStarveTheWatchdogTickle fails if another open of
// the blink(1)'s node that is stopped inside one command (a `ghost-wispr
// light-state` stopped between its SET_FEATURE and GET_FEATURE, by Ctrl-Z or
// `kill -STOP`) keeps the light controller from tickling the watchdog for
// longer than the timeout each tickle sets (serverDownTimeout): the blink(1)
// then plays its stored pattern while the recorder runs. The red team's test
// on #79.
func TestAStoppedHolderDoesNotStarveTheWatchdogTickle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *os.File {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}
	recorderFile, otherFile := open(), open()
	recorder := &blink1{file: recorderFile, patternLines: 32}
	other := &blink1{file: otherFile, patternLines: 32}
	otherFd := otherFile.Fd()

	var (
		mu      sync.Mutex
		colour  [3]byte
		tickles []time.Time
	)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	resume := make(chan struct{})
	var resumeOnce sync.Once
	continueOther := func() { resumeOnce.Do(func() { close(resume) }) }

	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	t.Cleanup(continueOther)
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		if fd == otherFd && request == hidIOCSetFeature {
			// The other process is stopped inside its command: its transfer
			// does not return until it is continued.
			stopOnce.Do(func() { close(stopped) })
			select {
			case <-resume:
			case <-time.After(10 * time.Second):
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if request == hidIOCGetFeature {
			switch report[1] {
			case 'r':
				report[2], report[3], report[4] = colour[0], colour[1], colour[2]
			case 'R':
				report[6] = 10 // a black line with a duration: the stored pattern is black
			}
			return nil
		}
		switch report[1] {
		case 'n':
			copy(colour[:], report[2:5])
		case 'D':
			if report[2] == 1 {
				tickles = append(tickles, time.Now())
			}
		}
		return nil
	}
	tickleCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(tickles)
	}
	waitForTickles := func(n int, within time.Duration) {
		t.Helper()
		for deadline := time.Now().Add(within); tickleCount() < n; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("control: %d watchdog tickles within %v, want %d", tickleCount(), within, n)
			}
		}
	}

	controller := NewController(recorder, func() bool { return true }, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		continueOther()
		cancel()
		<-ran
	})

	// Control: the controller tickles the watchdog about once a second.
	waitForTickles(2, 3*time.Second)
	// Right after a tickle, light-state starts a read and is stopped inside it for 3.5 s.
	waitForTickles(tickleCount()+1, 2*time.Second)
	otherDone := make(chan error, 1)
	go func() {
		_, _, _, err := other.ReadRGB()
		otherDone <- err
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("control: light-state's read never reached the blink(1)")
	}
	time.Sleep(3500 * time.Millisecond)
	continueOther()
	// Once continued, light-state's read ends: its GET may return the echo of
	// a tickle the recorder sent meanwhile, which is not its reply.
	if err := <-otherDone; err != nil && !errors.Is(err, ErrOtherReply) {
		t.Fatalf("control: light-state's read once continued: %v", err)
	}
	time.Sleep(1500 * time.Millisecond) // room for the controller to come back

	mu.Lock()
	defer mu.Unlock()
	var worst time.Duration
	for i := 1; i < len(tickles); i++ {
		worst = max(worst, tickles[i].Sub(tickles[i-1]))
	}
	t.Logf("%d watchdog tickles; the longest time between two was %v", len(tickles), worst.Round(10*time.Millisecond))
	if worst > serverDownTimeout {
		t.Fatalf("no watchdog tickle for %v while another open of the node was stopped inside a command; each tickle sets a %v timeout, so the blink(1) played its stored pattern while the recorder ran", worst.Round(10*time.Millisecond), serverDownTimeout)
	}
}
