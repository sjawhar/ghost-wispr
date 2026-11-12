package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// sweepStoreStub has one session waiting for its first sync, every time.
type sweepStoreStub struct{}

func (sweepStoreStub) GetSessionsNeedingSync() ([]string, error) {
	return []string{"20261106100000"}, nil
}

func (sweepStoreStub) GetSessionsBySyncState(string) ([]storage.Session, error) { return nil, nil }

func (sweepStoreStub) OwedDriveReplacements() ([]storage.OwedDriveReplacement, error) {
	return nil, nil
}

func (sweepStoreStub) RetryOwedErasure() error { return nil }

// erasureRetryStore counts the retries of owed erasures it is asked for.
type erasureRetryStore struct {
	sweepStoreStub
	retries atomic.Int64
}

func (s *erasureRetryStore) RetryOwedErasure() error {
	s.retries.Add(1)
	return nil
}

// TestTheSyncSweepRetriesOwedErasures fails if the sync sweep does not retry,
// on each pass and with Drive sync off, the erase a deletion still owes the
// write-ahead log and the pre-migration backup.
func TestTheSyncSweepRetriesOwedErasures(t *testing.T) {
	store := &erasureRetryStore{}
	ctx, cancel := context.WithCancel(context.Background())
	swept := goDone(func() { sweepDriveSync(ctx, store, &driveSync{}, newWarningsRegistry(), time.Millisecond) })
	t.Cleanup(func() {
		cancel()
		<-swept
	})
	deadline := time.Now().Add(5 * time.Second)
	for store.retries.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.retries.Load(); got < 2 {
		t.Fatalf("the sweep retried owed erasures %d times with sync off, want one per pass", got)
	}
}

// countingSyncer counts the syncs it is asked for.
type countingSyncer struct{ calls atomic.Int64 }

func (c *countingSyncer) SyncSession(context.Context, string) error {
	c.calls.Add(1)
	return nil
}

func (c *countingSyncer) CanSync(context.Context, string) error { return nil }

func (c *countingSyncer) FolderOnDrive(context.Context, string) (bool, error) { return true, nil }

// Review of #37, round 2, note 6 (on main since #13): turning Drive sync off
// in the settings while the app runs cleared the syncer while the sync sweep
// still called it: a data race, and a nil call that killed the process. The
// sweep keeps running through sync turned off and on, and syncs nothing while
// it is off.
func TestDriveSyncSweepSurvivesSyncTurnedOffAndOn(t *testing.T) {
	drive := &driveSync{}
	syncer := &countingSyncer{}
	drive.Set(syncer)
	ctx, cancel := context.WithCancel(context.Background())
	swept := goDone(func() { sweepDriveSync(ctx, sweepStoreStub{}, drive, newWarningsRegistry(), time.Millisecond) })
	t.Cleanup(func() {
		cancel()
		<-swept
	})

	for range 200 {
		drive.Set(nil)
		time.Sleep(100 * time.Microsecond)
		drive.Set(syncer)
		time.Sleep(100 * time.Microsecond)
	}
	if syncer.calls.Load() == 0 {
		t.Fatal("the sweep never synced while sync was on")
	}

	drive.Set(nil)
	time.Sleep(20 * time.Millisecond) // a sync under way when sync was turned off finishes
	off := syncer.calls.Load()
	time.Sleep(20 * time.Millisecond)
	if n := syncer.calls.Load(); n != off {
		t.Errorf("the sweep synced %d sessions while sync was off", n-off)
	}

	cancel()
	if !returnedWithin(swept, testWait) {
		t.Fatal("sweep still running after shutdown")
	}
}

// Review of #37, round 3, note 2 (on main too): two settings changes at once
// (sync on, then off) ran their callbacks concurrently, and the one turning
// sync on, slow to start its syncer, set it after the one turning sync off
// had found nothing to stop: sync kept running while the config said off,
// and GC, which follows the config, ran ungated beside it. The running sync
// now follows the last config saved.
func TestDriveSyncFollowsTheLastOfTwoConcurrentSettingsChanges(t *testing.T) {
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatalf("config store: %v", err)
	}
	if err := cfgStore.Update(func(c *config.Config) { c.GDriveFolderID = "folder" }); err != nil {
		t.Fatalf("set the folder: %v", err)
	}
	db, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	drive := &driveSync{}
	starting, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	watchDriveSyncSettings(cfgStore, drive, func(*config.Config) (sessionSyncer, error) {
		once.Do(func() { close(starting) })
		<-release // starting a syncer talks to Drive: slow
		return &countingSyncer{}, nil
	}, func(sessionSyncer) {})
	h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), db, &server.ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	patch := func(body string) <-chan struct{} {
		return goDone(func() {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body)))
			if rr.Code != http.StatusOK {
				t.Errorf("PATCH %s: %d %s", body, rr.Code, rr.Body.String())
			}
		})
	}

	on := patch(`{"gdrive":{"sync_enabled":true}}`)
	<-starting
	off := patch(`{"gdrive":{"sync_enabled":false}}`)
	for deadline := time.Now().Add(testWait); cfgStore.Get().GDriveSyncEnabled; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("sync never turned off in the config")
		}
	}
	close(release)
	if !returnedWithin(on, testWait) || !returnedWithin(off, testWait) {
		t.Fatal("settings changes still running")
	}

	if cfgStore.Get().GDriveSyncEnabled {
		t.Fatal("the last change saved was sync off")
	}
	if drive.Get() != nil {
		t.Error("Drive sync is running, but the last settings change turned it off")
	}
}
