package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// wireSocket is a socket that takes all it is sent, and keeps the stream the
// client gives it (transcribe.StreamedSocket).
type wireSocket struct {
	mu     sync.Mutex
	wire   []byte
	stream *transcribe.Stream
}

func (s *wireSocket) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wire = append(s.wire, p...)
	return len(p), nil
}

func (s *wireSocket) Close() error                        { return nil }
func (s *wireSocket) UseStream(stream *transcribe.Stream) { s.stream = stream }

func (s *wireSocket) got() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.wire)
}

// refusingDrain is a socket whose first write waits for release, then fails
// having taken nothing.
type refusingDrain struct {
	entered, release chan struct{}
	once             sync.Once
}

func (s *refusingDrain) Write([]byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return 0, errors.New("the drain write was refused")
}

func (s *refusingDrain) Close() error { return nil }

// TestARefusedChunkIsRecordedThroughALongOutage fails if the chunk a socket
// refused at the start of a long outage, with the default 60 s buffer, is not
// sent whole whatever its age, as main sends it, or is sent while the history
// the session keeps could hold it but the recording does not. The session
// keeps recording history as far back as MaxBehind, twice the buffer and a
// drain chunk, and a 30 s margin for late results: a word Deepgram hears in
// the chunk, even 30 s after the reconnect, is placed at its capture time in
// a recording that holds its audio. A chunk 125.064 s behind when its result
// comes 30 s late is 0.064 s inside that history. Past it the chunk is still
// sent, as on main, but no recording holds it. The chunk's first 256 bytes
// mark it in the recording. From the reviews of #157, which lost the chunk at
// 60.25 s and at 120.002 s, and the review of #130, whose recording at 120 s
// did not hold it.
func TestARefusedChunkIsRecordedThroughALongOutage(t *testing.T) {
	runParallel(t)
	for _, c := range []struct {
		name          string
		outage, late  time.Duration
		recordsPrefix bool
	}{
		{name: "60.5 s", outage: 60500 * time.Millisecond, recordsPrefix: true},
		{name: "125.064 s, the result 30 s late", outage: 125064 * time.Millisecond, late: 30 * time.Second, recordsPrefix: true},
		{name: "170 s, past the history", outage: 170 * time.Second, recordsPrefix: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusedChunkThroughOutage(t, c.outage, c.late, c.recordsPrefix)
		})
	}
}

func refusedChunkThroughOutage(t *testing.T, outage, late time.Duration, recordsPrefix bool) {
	dir := t.TempDir()
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "outage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rec := audio.NewRecorder(dir)
	if err := rec.SetSampleRate(16000); err != nil {
		t.Fatal(err)
	}
	manager := session.NewManager(store, rec, nil, nil, session.NewDetector(time.Hour), 0)
	t.Cleanup(func() { _ = manager.StopBackground(context.Background()) })
	refused := &refusingDrain{entered: make(chan struct{}), release: make(chan struct{})}
	next := &wireSocket{}
	dials := 0
	factory := func(context.Context) (io.WriteCloser, error) {
		dials++
		if dials == 1 {
			return refused, nil
		}
		return next, nil
	}
	rc := transcribe.NewResilientClient(context.Background(), factory, transcribe.ResilientConfig{InitialReconnectDelay: time.Millisecond, MaxReconnectBackoff: time.Millisecond}, nil, nil)
	t.Cleanup(func() { _ = rc.Close() })
	manager.SetLiveStream(rc)
	writer := rec.Writer(rc)

	const bytesPerSecond = 32000
	n := int(outage * bytesPerSecond / time.Second)
	captured := make([]byte, n+int(late*bytesPerSecond/time.Second))
	for i := range 4096 {
		captured[i] = byte(i*13 + i/127 + 1) // the refused chunk; the rest is silence
	}
	write := func(from, to int) {
		for at := from; at < to; at += 8000 {
			if _, err := writer.Write(captured[at:min(at+8000, to)]); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(0, 8000) // 250 ms: the drain takes the first 4096 bytes
	rc.ConnectInBackground()
	awaitClosedWithin(t, refused.entered, "the drain never wrote to the first socket")
	write(8000, n) // the outage: the buffer keeps its newest 60 s
	close(refused.release)
	waitForHardwareWithin(t, 10*time.Second, "the next socket", rc.IsConnected)
	write(n, len(captured)) // captured until Deepgram's result comes

	bytesTime := func(b int) time.Duration { return time.Duration(b) * time.Second / bytesPerSecond }
	tailStart := n - 1920000 // the buffer keeps the newest 60 s
	want := append(bytes.Clone(captured[:4096]), captured[tailStart:]...)
	if got := next.got(); !bytes.Equal(got, want) {
		t.Fatalf("the next socket got %d bytes, want %d: the whole refused chunk, then the buffer's 60 s and what came after", len(got), len(want))
	}
	if got := next.stream.CapturedBefore(0); got != 0 {
		t.Errorf("the next socket's stream starts at capture %v, want 0, the refused chunk", got)
	}
	if got, want := next.stream.CapturedBefore(bytesTime(4096)), bytesTime(tailStart); got != want {
		t.Errorf("the audio after the chunk maps to capture %v, want %v, past the hole", got, want)
	}

	var result api.MessageResponse
	if err := json.Unmarshal(finalResult("Recovered.", 0.01), &result); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MessageKeptOn(next.stream, &result); err != nil {
		t.Fatal(err)
	}
	id, _ := manager.ActiveSession()
	if id == "" {
		t.Fatal("the result opened no session")
	}
	raw, err := os.ReadFile(recording.RawPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Contains(raw, captured[:256]); got != recordsPrefix {
		t.Errorf("the session's recording of %d bytes holds the refused chunk %t, want %t", len(raw), got, recordsPrefix)
	}
	segments, err := store.GetSegments(id)
	if err != nil || len(segments) != 1 || segments[0].Text != "Recovered." {
		t.Errorf("the session stored %+v (%v), want the one word", segments, err)
	}
	if _, err := rec.EndSession(id); err != nil {
		t.Fatal(err)
	}
}
