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
// a collector sweeping without pause at a 1 MB budget: after every write the
// live raw recording is whole, and every session keeps its row and either its
// recording on disk or the marker saying GC removed it, never a path to a
// file that is gone.
func TestGCNeverTouchesTheLiveRecording(t *testing.T) {
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
		collector := gc.New(store, gc.Config{MaxAgeDays: 30, MaxAudioSizeMB: 1, AudioDir: audioDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		for ctx.Err() == nil {
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

// With Drive sync on in the config, GC is sync-gated whether or not the
// syncer started: when it fails to start (credentials unreadable, say), the
// age policy still deletes only sessions synced to Drive, and disk pressure
// removes only their audio. It used to run ungated then, deleting sessions
// never backed up.
func TestGCStaysSyncGatedWhenDriveSyncIsOnButNotRunning(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i, id := range []string{"20261001120000", "20261001130000"} {
		started := old.Add(time.Duration(i) * time.Hour)
		if err := store.CreateSession(id, started); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), ""); err != nil {
			t.Fatalf("end %s: %v", id, err)
		}
		if err := store.UpdateSummary(id, "", "A summary.", storage.SummaryCompleted, "default", "{}"); err != nil {
			t.Fatalf("summarize %s: %v", id, err)
		}
	}
	if err := store.UpdateSyncStatus("20261001130000", storage.SyncSynced, "folder"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	cfg := config.Config{GCEnabled: true, GCMaxAgeDays: 30, GCMaxAudioSizeMB: 1024, GDriveSyncEnabled: true, AudioDir: t.TempDir()}
	gcCfg := gcConfig(&cfg) // no syncer: it failed to start
	if !gcCfg.SyncGated {
		t.Errorf("Drive sync on but not running: GC sync gate off")
	}
	deleted, err := gc.New(store, gcCfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Run()
	if err != nil {
		t.Fatalf("gc run: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "20261001130000" {
		t.Errorf("deleted %v, want only the session synced to Drive, [20261001130000]", deleted)
	}
	if _, err := store.GetSession("20261001120000"); err != nil {
		t.Errorf("the session never backed up was deleted: %v", err)
	}
}
