package audio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"testing"
	"time"
)

// TestAnOversizedCaptureIsNotSaved fails if the speaker trace, with its
// production limits, saves a snapshot larger than a capture's byte budget, or
// leaves any of it on disk. A burst of 40,000 trace events of 1 KiB each makes
// a trace generation of about 41 MB, past the 32 MiB budget, against the
// flight recorder's 16 MiB hint. The control, a snapshot of a quiet process,
// is saved.
func TestAnOversizedCaptureIsNotSaved(t *testing.T) {
	dir := t.TempDir()
	st, err := StartSpeakerTrace(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Stop()
	if st.fileBytes != speakerTraceFileBytes {
		t.Fatalf("control: the capture budget is %d bytes, want the production %d", st.fileBytes, speakerTraceFileBytes)
	}

	quiet, err := st.save(time.Now())
	if err != nil {
		t.Fatalf("control: a quiet snapshot was not saved: %v", err)
	}
	if err := os.Remove(quiet); err != nil {
		t.Fatal(err)
	}

	message := strings.Repeat("x", 1024)
	for i := range 40_000 {
		trace.Log(traceRegions, "burst", message[:1024-len(fmt.Sprint(i))]+fmt.Sprint(i))
	}
	path, err := st.save(time.Now().Add(time.Second))
	if !errors.Is(err, errCaptureTooLarge) {
		t.Fatalf("a burst's snapshot: save = %q, %v; want %v", path, err, errCaptureTooLarge)
	}
	left, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("the oversized capture left %q on disk, want nothing", left)
	}
}

// TestTheCapturesKeptFitTheDirectoryBudget fails if, after a save, the
// captures kept in the directory are more than the newest that fit its byte
// budget together, or if the save removes the new capture or a file that is
// not a capture. Six old captures of 30 MiB each (sparse files) and the new
// one fit the 128 MiB budget only as the new one and the newest four. The new
// capture's name sorts before the old ones, as after the Pi, which has no
// clock of its own, steps its time back at time sync: it is kept all the same.
func TestTheCapturesKeptFitTheDirectoryBudget(t *testing.T) {
	dir := t.TempDir()
	st, err := StartSpeakerTrace(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Stop()
	if st.dirBytes != speakerTraceDirBytes {
		t.Fatalf("control: the directory budget is %d bytes, want the production %d", st.dirBytes, speakerTraceDirBytes)
	}
	start := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	var old []string
	for i := range 6 {
		p := filepath.Join(dir, "speaker-underrun-"+start.Add(time.Duration(i)*time.Minute).Format("20060102T150405.000Z")+".trace")
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(30 << 20); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		old = append(old, p)
	}
	unrelated := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(unrelated, []byte("not a capture"), 0o600); err != nil {
		t.Fatal(err)
	}

	saved, err := st.save(start.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range old {
		_, err := os.Stat(p)
		if kept := err == nil; kept != (i >= 2) {
			t.Errorf("old capture %d of 6 kept %t, want only the newest four kept", i+1, kept)
		}
	}
	for _, p := range []string{saved, unrelated} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}
