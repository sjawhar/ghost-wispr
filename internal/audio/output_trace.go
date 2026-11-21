package audio

import (
	"context"
	"fmt"
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

// The speaker trace's limits: it keeps at least speakerTraceWindow of trace,
// up to speakerTraceBytes; it saves at most once every speakerTraceEvery,
// speakerTraceDelay after the underrun so the capture holds the write that
// found the device dry; and it keeps the newest speakerTraceKeep captures.
const (
	speakerTraceWindow = 3 * time.Second
	speakerTraceBytes  = 16 << 20
	speakerTraceEvery  = 10 * time.Second
	speakerTraceDelay  = 250 * time.Millisecond
	speakerTraceKeep   = 20
)

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
	fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{MinAge: speakerTraceWindow, MaxBytes: speakerTraceBytes})
	if err := fr.Start(); err != nil {
		return nil, fmt.Errorf("speaker trace: start the flight recorder: %w", err)
	}
	return &SpeakerTrace{dir: dir, fr: fr, every: speakerTraceEvery}, nil
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

// save writes the flight recorder's window to a new file and removes the
// oldest captures beyond speakerTraceKeep.
func (s *SpeakerTrace) save(at time.Time) (string, error) {
	path := filepath.Join(s.dir, "speaker-underrun-"+at.UTC().Format("20060102T150405.000Z")+".trace")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := s.fr.WriteTo(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	old, err := filepath.Glob(filepath.Join(s.dir, "speaker-underrun-*.trace"))
	if err == nil && len(old) > speakerTraceKeep {
		slices.SortFunc(old, strings.Compare) // the names sort by time
		for _, p := range old[:len(old)-speakerTraceKeep] {
			_ = os.Remove(p)
		}
	}
	return path, nil
}

// Stop waits for a capture being saved and stops the flight recorder.
func (s *SpeakerTrace) Stop() {
	s.saving.Wait()
	s.fr.Stop()
}
