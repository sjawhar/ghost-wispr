package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"
)

// The trace regions the mixer marks in an execution trace, which a speaker
// trace (SpeakerTrace) captures and internal/speakertrace reads: its write of
// one frame to the stream, the pacer holding it back to real time, and its
// wait for the Output's lock. A region costs nothing while no trace runs.
const (
	regionWrite = "speaker.write"
	regionPacer = "speaker.pacer"
	regionLock  = "speaker.lock"
)

// traceRegions is the context the mixer opens its regions on.
var traceRegions = context.Background()

// The mixer marks its regions with trace.WithRegion, which allocates nothing.
// trace.StartRegion allocates a Region while a trace runs, and a mixer that
// allocates during a collection's mark phase is made to help mark and to wait
// for the phase to end: the trace would put the mixer in waits it never has
// without it (TestTheMixersRegionsAllocateNothing).

// lockForMix takes the Output's lock for the mixer, inside regionLock, so a
// trace shows how long the mixer waited for it and who held it.
func (o *Output) lockForMix() {
	trace.WithRegion(traceRegions, regionLock, o.mu.Lock)
}

// writeFrame writes frame to stream inside regionWrite.
func writeFrame(stream PlaybackStream, frame []int16) (err error) {
	trace.WithRegion(traceRegions, regionWrite, func() { err = stream.Write(frame) })
	return err
}

// pacerSleep sleeps d inside regionPacer.
func pacerSleep(d time.Duration) {
	trace.WithRegion(traceRegions, regionPacer, func() { time.Sleep(d) })
}

// The speaker trace's limits. The flight recorder keeps at least
// speakerTraceWindow of trace, aiming at speakerTraceHint bytes: a target the
// Go runtime may exceed, since it keeps whole generations of trace, so a burst
// of events can make a snapshot far larger (41.5 MB against the 16 MiB hint
// in one review probe). A capture is saved only if its snapshot fits
// speakerTraceFileBytes, twice the hint, and the captures kept in the
// directory are the newest speakerTraceKeep that fit speakerTraceDirBytes
// together: the Pi's SD card holds the recordings too. It saves at most once
// every speakerTraceEvery, speakerTraceDelay after the underrun so the capture
// holds the write that found the device dry.
const (
	speakerTraceWindow    = 3 * time.Second
	speakerTraceHint      = 16 << 20
	speakerTraceFileBytes = 2 * speakerTraceHint
	speakerTraceDirBytes  = 128 << 20
	speakerTraceEvery     = 10 * time.Second
	speakerTraceDelay     = 250 * time.Millisecond
	speakerTraceKeep      = 20
)

// errCaptureTooLarge is a snapshot larger than a capture's byte budget: it is
// not saved, and nothing of it is left on disk.
var errCaptureTooLarge = errors.New("the speaker trace's snapshot is larger than a capture may be")

// SpeakerTrace keeps the last seconds of the process's execution trace in
// memory (runtime/trace's flight recorder) and writes them to a file in dir
// when the speaker underruns: the speaker_trace_dir setting, off by default,
// for finding what keeps the mixer from the device. `ghost-wispr
// speaker-trace FILE` reads a capture. Only one flight recorder can run in a
// process.
type SpeakerTrace struct {
	dir   string
	fr    *trace.FlightRecorder
	every time.Duration
	// fileBytes and dirBytes are the byte budgets of one capture and of the
	// captures kept in dir (speakerTraceFileBytes, speakerTraceDirBytes).
	fileBytes, dirBytes int64

	mu     sync.Mutex
	last   time.Time
	saving sync.WaitGroup
	// saved, when set, gets each capture's path (tests).
	saved func(path string)
}

// StartSpeakerTrace starts the flight recorder for captures in dir.
func StartSpeakerTrace(dir string) (*SpeakerTrace, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("speaker trace: %w", err)
	}
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: speakerTraceWindow, MaxBytes: speakerTraceHint})
	if err := fr.Start(); err != nil {
		return nil, fmt.Errorf("speaker trace: start the flight recorder: %w", err)
	}
	return &SpeakerTrace{dir: dir, fr: fr, every: speakerTraceEvery, fileBytes: speakerTraceFileBytes, dirBytes: speakerTraceDirBytes}, nil
}

// Underrun saves a capture in the background, speakerTraceDelay from now,
// unless one was saved less than every ago. It never blocks the mixer.
func (s *SpeakerTrace) Underrun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !s.last.IsZero() && now.Sub(s.last) < s.every {
		return
	}
	s.last = now
	s.saving.Add(1)
	go func() {
		defer s.saving.Done()
		time.Sleep(speakerTraceDelay)
		path, err := s.save(now)
		if err != nil {
			slog.Warn("speaker trace: the capture was not saved", "error", err)
			return
		}
		slog.Info("speaker trace: saved the capture of an underrun", "path", path)
		if s.saved != nil {
			s.saved(path)
		}
	}()
}

// save writes the flight recorder's window to a new file, unless it is
// larger than fileBytes: then it fails with errCaptureTooLarge and removes
// what it wrote. It then removes the oldest captures until the newest
// speakerTraceKeep are left and they fit dirBytes together; the new capture
// is always kept.
func (s *SpeakerTrace) save(at time.Time) (string, error) {
	path := filepath.Join(s.dir, "speaker-underrun-"+at.UTC().Format("20060102T150405.000Z")+".trace")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := s.fr.WriteTo(&budgetWriter{w: f, left: s.fileBytes}); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	s.prune(path)
	return path, nil
}

// prune removes the oldest captures in dir, never kept (the one just saved),
// until at most speakerTraceKeep are left and their sizes add up to at most
// dirBytes.
func (s *SpeakerTrace) prune(kept string) {
	captures, err := filepath.Glob(filepath.Join(s.dir, "speaker-underrun-*.trace"))
	if err != nil {
		return
	}
	slices.SortFunc(captures, strings.Compare) // the names sort by time
	sizes := make([]int64, len(captures))
	var total int64
	for i, p := range captures {
		if info, err := os.Stat(p); err == nil {
			sizes[i] = info.Size()
			total += sizes[i]
		}
	}
	count := len(captures)
	for i, p := range captures {
		if count <= speakerTraceKeep && total <= s.dirBytes {
			return
		}
		if p == kept {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("speaker trace: an old capture could not be removed", "path", p, "error", err)
			continue
		}
		count--
		total -= sizes[i]
	}
}

// budgetWriter writes to w until a write would take it past left bytes: that
// write writes nothing and fails with errCaptureTooLarge.
type budgetWriter struct {
	w    io.Writer
	left int64
}

func (b *budgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.left {
		return 0, errCaptureTooLarge
	}
	n, err := b.w.Write(p)
	b.left -= int64(n)
	return n, err
}

// Stop waits for a capture being saved and stops the flight recorder.
func (s *SpeakerTrace) Stop() {
	s.saving.Wait()
	s.fr.Stop()
}
