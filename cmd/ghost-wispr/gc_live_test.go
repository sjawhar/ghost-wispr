package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gc"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// Reviews of #36, rounds 1 to 4: GC never touches the recording under way.
// Twenty sessions through the real Manager, Recorder and SQLiteStore, against
// a Drive-gated collector sweeping without pause at a 1 MB budget. Each
// session is marked synced as soon as it ends and the verifier confirms its
// Drive copy, so GC removes audio under pressure. After every write the live
// raw recording is whole, and every session keeps its row and either its
// recording on disk or the marker saying GC removed it, never a path to a
// file that is gone.
func TestGCNeverTouchesTheLiveRecording(t *testing.T) {
	// This test exercises GC under a fixed, uncompressed 5 MB recording load;
	// external encoders would make the same zero-valued PCM compress below its
	// 1 MB budget and turn its pressure assertion into a no-op.
	t.Setenv("PATH", t.TempDir())
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	recorder := audio.NewRecorder(audioDir)
	manager := session.NewManager(store, recorder, nil, nil, session.NewDetector(time.Hour), 0)
	mic := recorder.Writer(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	var passes atomic.Int64
	swept := goDone(func() {
		collector := gc.New(store, gc.Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: audioDir, Drive: &countingSyncer{}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		for ctx.Err() == nil {
			// A sync that completed: ended sessions are synced into a folder.
			if _, err := store.DB().Exec(`UPDATE sessions SET sync_state = ?, sync_status = ?, gdrive_folder_id = 'folder'
				WHERE status = ? AND sync_state = ?`, storage.SyncStateSynced, storage.SyncSynced, storage.SessionEnded, storage.SyncStatePending); err != nil {
				t.Errorf("mark ended sessions synced: %v", err)
				return
			}
			if _, err := collector.Run(); err != nil {
				t.Errorf("gc run: %v", err)
				return
			}
			passes.Add(1)
		}
	})
	stopSweeping := func() {
		cancel()
		if !returnedWithin(swept, testWait) {
			t.Fatal("collector still sweeping")
		}
	}
	defer stopSweeping()

	chunk := make([]byte, 4096)
	for i := range chunk {
		chunk[i] = byte(i*31 + i/7)
	}
	var ids []string
	for range 20 {
		id, err := manager.ManualStartSession(context.Background(), "")
		if err != nil {
			t.Fatalf("start session: %v", err)
		}
		ids = append(ids, id)
		live := filepath.Join(audioDir, id+".pcm")
		var written int64
		for range 64 {
			if _, err := mic.Write(chunk); err != nil {
				t.Fatalf("write audio: %v", err)
			}
			written += int64(len(chunk))
			info, err := os.Stat(live)
			if err != nil {
				t.Fatalf("GC removed the recording under way: %v", err)
			}
			if info.Size() < written {
				t.Fatalf("the recording under way has %d bytes, want at least %d", info.Size(), written)
			}
		}
		if err := manager.StopSession(context.Background(), id); err != nil {
			t.Fatalf("stop session %s: %v", id, err)
		}
	}
	stopSweeping()

	if passes.Load() == 0 {
		t.Fatal("the collector never completed a pass")
	}
	removed := 0
	for _, id := range ids {
		sess, err := store.GetSession(id)
		if err != nil {
			t.Errorf("session %s deleted: %v", id, err)
			continue
		}
		if sess.AudioPath == "" {
			if sess.AudioRemovedAt == "" {
				t.Errorf("session %s has no recording and no audio-removed marker", id)
			}
			removed++
			continue
		}
		for _, entry := range strings.Split(sess.AudioPath, ",") {
			if _, err := os.Stat(entry); err != nil {
				t.Errorf("session %s names %s, which is gone: %v", id, entry, err)
			}
		}
	}
	if removed == 0 {
		t.Errorf("GC removed no recording under a 1 MB budget with %d sessions of 256 KB; the test did not put it under pressure", len(ids))
	}
	t.Logf("%d collector passes; %d of %d sessions had their recording removed to free space", passes.Load(), removed, len(ids))
}

// GC deletes and removes audio only after Drive answers that it has the
// session folder, out of the trash. When the syncer failed to start
// (credentials unreadable, say), nothing can ask Drive, so GC deletes nothing
// and removes no audio: a synced session's folder may be in the trash since,
// which Drive empties after 30 days. GC used to run ungated then, deleting
// sessions never backed up, and later deleted the synced ones without asking
// Drive.
func TestGCStaysDriveGatedWhenSyncIsNotRunning(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	audioDir := t.TempDir()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	ids := []string{"20261001120000", "20261001130000"}
	var recordings []string
	for i, id := range ids {
		started := old.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, started); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		recording := filepath.Join(audioDir, id+".mp3")
		if err := os.WriteFile(recording, make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		recordings = append(recordings, recording)
		if err := store.EndSession(id, started.Add(time.Minute), recording); err != nil {
			t.Fatalf("end %s: %v", id, err)
		}
		if err := store.UpdateSummaryAtGeneration(id, "", "A summary.", storage.SummaryCompleted, "default", "{}", "", 0); err != nil {
			t.Fatalf("summarize %s: %v", id, err)
		}
	}
	syncTo(t, store, "20261001130000", storage.SyncStateSynced, "folder", 0, "")

	cfg := config.Config{GCEnabled: true, GCMaxAgeDays: 30, GCMaxAudioSizeMB: 1, GDriveSyncEnabled: true, AudioDir: audioDir}
	gcCfg := gcConfig(&cfg, nil) // no syncer: it failed to start
	deleted, err := gc.New(store, gcCfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("deleted %v, want none: nothing can check that Drive still has them", deleted)
	}
	for _, id := range ids {
		if _, err := store.GetSession(id); err != nil {
			t.Errorf("session %s deleted: %v", id, err)
		}
	}
	for _, recording := range recordings {
		if _, err := os.Stat(recording); err != nil {
			t.Errorf("recording removed: %v", err)
		}
	}
}
