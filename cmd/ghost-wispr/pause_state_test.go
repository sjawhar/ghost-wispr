package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type pauseStoreFake struct {
	values map[string]string
	err    error
}

func (s *pauseStoreFake) GetAppState(key string) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	value, ok := s.values[key]
	return value, ok, nil
}

func (s *pauseStoreFake) SetAppState(key, value string) error {
	if s.err != nil {
		return s.err
	}
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

type pauseGateFake struct{ calls int }

func (g *pauseGateFake) Pause() { g.calls++ }

// TestRestorePausePausesWhenStored fails if a persisted pause is not restored
// before capture begins.
func TestRestorePausePausesWhenStored(t *testing.T) {
	for _, tc := range []struct {
		name       string
		values     map[string]string
		wantPaused bool
		wantErr    bool
	}{
		{name: "paused", values: map[string]string{storage.AppStateCapturePaused: "1"}, wantPaused: true},
		{name: "resumed", values: map[string]string{storage.AppStateCapturePaused: "0"}},
		{name: "missing", values: map[string]string{}},
		{name: "invalid", values: map[string]string{storage.AppStateCapturePaused: "invalid"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &pauseGateFake{}
			paused, err := restorePause(&pauseStoreFake{values: tc.values}, gate)
			if (err != nil) != tc.wantErr {
				t.Fatalf("restore pause error = %v, want error=%t", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if paused != tc.wantPaused {
				t.Fatalf("restore paused = %t, want %t", paused, tc.wantPaused)
			}
			if got := gate.calls; got != map[bool]int{true: 1, false: 0}[tc.wantPaused] {
				t.Fatalf("Pause calls = %d, want %d", got, map[bool]int{true: 1, false: 0}[tc.wantPaused])
			}
		})
	}
}

// TestPersistingControlsWritesBeforeReturning fails if a control reply can
// report success before its restart-safe state reaches the store.
func TestPersistingControlsWritesBeforeReturning(t *testing.T) {
	store := &pauseStoreFake{}
	var pauses, resumes int
	controls := newPersistentControls(store, func() { pauses++ }, func() error {
		resumes++
		return nil
	}, nil)
	pause, resume := controls.Pause, controls.Resume

	if err := pause(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := store.values[storage.AppStateCapturePaused]; got != "1" {
		t.Fatalf("stored state after pause = %q, want 1", got)
	}
	if pauses != 1 {
		t.Fatalf("pause calls = %d, want 1", pauses)
	}
	if err := resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := store.values[storage.AppStateCapturePaused]; got != "0" {
		t.Fatalf("stored state after resume = %q, want 0", got)
	}
	if resumes != 1 {
		t.Fatalf("resume calls = %d, want 1", resumes)
	}
}

// TestAStoreWriteFailureIsReported fails if a failed resume store write lets
// capture resume, or a failed pause write leaves capture running.
func TestAStoreWriteFailureIsReported(t *testing.T) {
	store := &pauseStoreFake{err: errors.New("disk full")}
	var pauses, resumes int
	controls := newPersistentControls(store, func() { pauses++ }, func() error {
		resumes++
		return nil
	}, nil)
	pause, resume := controls.Pause, controls.Resume

	if err := pause(); err == nil {
		t.Fatal("pause succeeded after storage failure")
	}
	if pauses != 1 {
		t.Fatalf("pause calls = %d, want 1", pauses)
	}
	if err := resume(); err == nil {
		t.Fatal("resume succeeded after storage failure")
	}
	if resumes != 0 {
		t.Fatalf("resume calls = %d, want 0", resumes)
	}
}

// TestPauseHTTPPersists fails if the public pause route bypasses the durable
// controls that the physical button uses.
func TestPauseHTTPPersists(t *testing.T) {
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capture := newCaptureGate(nil, nil, nil)
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, nil)
	pause, resume := controls.Pause, controls.Resume
	handler, err := server.Handler(
		fstest.MapFS{"index.html": {Data: []byte("ok")}},
		server.NewHub(),
		store,
		&server.ControlHooks{Pause: pause, Resume: resume, IsPaused: capture.IsPaused},
		"", nil,
	)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/pause", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("pause status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if err != nil || !ok || value != "1" {
		t.Fatalf("stored pause = (%q, %t, %v), want (1, true, nil)", value, ok, err)
	}
}

// TestRestoredPauseOpensNoMicrophone fails if a persisted pause starts audio
// capture before a person explicitly resumes it.
func TestRestoredPauseOpensNoMicrophone(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	paused, err := restorePause(&pauseStoreFake{values: map[string]string{storage.AppStateCapturePaused: "1"}}, capture)
	if err != nil || !paused {
		t.Fatalf("restore pause = (%t, %v), want (true, nil)", paused, err)
	}
	mic := newScriptedMic()
	if err := startMicIfUnpaused(capture, mic.Start); err != nil {
		t.Fatalf("start while restored paused: %v", err)
	}
	mic.expectNoEvents(t)
	capture.Resume()
	if err := startMicIfUnpaused(capture, mic.Start); err != nil {
		t.Fatalf("start after resume: %v", err)
	}
	mic.expectEvents(t, "start")
}

// TestStartMicIfUnpausedConfirmsRedBeforeStarting fails if boot can start
// capture before the indicator has confirmed red.
func TestStartMicIfUnpausedConfirmsRedBeforeStarting(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	var calls []string
	capture.beforeMicStart = func() error {
		calls = append(calls, "confirm-red")
		return nil
	}
	capture.afterMicStart = func() { calls = append(calls, "finish-start") }
	if err := startMicIfUnpaused(capture, func() error {
		calls = append(calls, "start")
		return nil
	}); err != nil {
		t.Fatalf("start microphone: %v", err)
	}
	if got, want := calls, []string{"confirm-red", "start", "finish-start"}; !slices.Equal(got, want) {
		t.Fatalf("microphone startup calls = %v, want %v", got, want)
	}
}

// TestStartMicIfUnpausedRefusesAnUnconfirmedLight fails if boot starts capture
// after the red check reports an error.
func TestStartMicIfUnpausedRefusesAnUnconfirmedLight(t *testing.T) {
	capture := newCaptureGate(nil, nil, nil)
	capture.beforeMicStart = func() error { return errors.New("light stayed off") }
	started := false
	if err := startMicIfUnpaused(capture, func() error {
		started = true
		return nil
	}); err == nil {
		t.Fatal("microphone start succeeded without a confirmed red light")
	}
	if started {
		t.Fatal("microphone start ran without a confirmed red light")
	}
}

// TestRefusedResumeHTTPReturns500 fails if the public resume route reports
// success after the pre-resume light confirmation refuses it.
func TestRefusedResumeHTTPReturns500(t *testing.T) {
	store, err := storage.NewSQLiteStore(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	capture.beforeResumeRequest = func() error { return errors.New("recording light stayed off") }
	controls := newPersistentControls(store, capture.Pause, capture.RequestResume, nil)
	handler, err := server.Handler(
		fstest.MapFS{"index.html": {Data: []byte("ok")}},
		server.NewHub(),
		store,
		&server.ControlHooks{Resume: controls.Resume, IsPaused: capture.IsPaused},
		"", nil,
	)
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/resume", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("refused resume status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	value, ok, err := store.GetAppState(storage.AppStateCapturePaused)
	if !capture.IsPaused() || err != nil || !ok || value != "1" {
		t.Fatalf("refused resume left paused=%t stored=(%q, %t, %v), want true and (1, true, nil)", capture.IsPaused(), value, ok, err)
	}
}

type blockingPauseStore struct {
	pauseStoreFake
	zeroStarted chan struct{}
	releaseZero chan struct{}
	once        sync.Once
}

func (s *blockingPauseStore) SetAppState(key, value string) error {
	if value == "0" {
		s.once.Do(func() { close(s.zeroStarted) })
		<-s.releaseZero
	}
	return s.pauseStoreFake.SetAppState(key, value)
}

// TestPersistentControlsSerializePauseAgainstResume fails if a shutdown pause
// can write state while a resume transaction is still in flight.
func TestPersistentControlsSerializePauseAgainstResume(t *testing.T) {
	store := &blockingPauseStore{
		zeroStarted: make(chan struct{}),
		releaseZero: make(chan struct{}),
	}
	controls := newPersistentControls(store, func() {}, func() error { return nil }, nil)
	resumeDone := make(chan error, 1)
	go func() { resumeDone <- controls.Resume() }()
	select {
	case <-store.zeroStarted:
	case <-time.After(time.Second):
		t.Fatal("resume did not begin its durable write")
	}
	pauseDone := make(chan error, 1)
	go func() { pauseDone <- controls.Pause() }()
	select {
	case err := <-pauseDone:
		t.Fatalf("pause wrote during a resume transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.releaseZero)
	if err := <-resumeDone; err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := <-pauseDone; err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := store.values[storage.AppStateCapturePaused]; got != "1" {
		t.Fatalf("final persisted state = %q, want 1", got)
	}
}

type refusedResumeWriteStore struct {
	pauseStoreFake
	writes int
}

func (s *refusedResumeWriteStore) SetAppState(key, value string) error {
	s.writes++
	if s.writes == 2 && value == "1" {
		return errors.New("disk full")
	}
	return s.pauseStoreFake.SetAppState(key, value)
}

// TestRefusedResumeWriteBackFailureIsReported fails if a refusal cannot restore
// the persisted pause yet callers cannot distinguish that restart risk.
func TestRefusedResumeWriteBackFailureIsReported(t *testing.T) {
	store := &refusedResumeWriteStore{}
	capture := newCaptureGate(nil, nil, nil)
	capture.Pause()
	capture.beforeResumeRequest = func() error { return errors.New("recording light stayed off") }
	var statuses []bool
	controls := newPersistentControls(
		store,
		capture.Pause,
		capture.RequestResume,
		func(paused bool) { statuses = append(statuses, paused) },
	)
	err := controls.Resume()
	if !errors.Is(err, errRefusedResumeNotPersisted) {
		t.Fatalf("resume error = %v, want errRefusedResumeNotPersisted", err)
	}
	if !capture.IsPaused() || store.values[storage.AppStateCapturePaused] != "0" {
		t.Fatalf("failed write-back left paused=%t stored=%q, want true and 0", capture.IsPaused(), store.values[storage.AppStateCapturePaused])
	}
	if got, want := statuses, []bool{true}; !slices.Equal(got, want) {
		t.Fatalf("status broadcasts = %v, want %v", got, want)
	}
}

// TestPersistentControlsAnnounceHTTPAndPowerTransitions fails if either public
// pause surface bypasses the controls' serialized status callback.
func TestPersistentControlsAnnounceHTTPAndPowerTransitions(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		store := &pauseStoreFake{}
		capture := newCaptureGate(nil, nil, nil)
		var statuses []bool
		controls := newPersistentControls(
			store,
			capture.Pause,
			capture.RequestResume,
			func(paused bool) { statuses = append(statuses, paused) },
		)
		handler, err := server.Handler(
			fstest.MapFS{"index.html": {Data: []byte("ok")}},
			server.NewHub(),
			&storage.SQLiteStore{},
			&server.ControlHooks{Pause: controls.Pause, Resume: controls.Resume},
			"", nil,
		)
		if err != nil {
			t.Fatalf("build handler: %v", err)
		}
		for _, path := range []string{"/api/pause", "/api/resume"} {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent {
				t.Fatalf("%s status = %d, want 204", path, response.Code)
			}
		}
		if got, want := statuses, []bool{true, false}; !slices.Equal(got, want) {
			t.Fatalf("HTTP status broadcasts = %v, want %v", got, want)
		}
	})

	t.Run("power button", func(t *testing.T) {
		store := &pauseStoreFake{}
		capture := newCaptureGate(nil, nil, nil)
		var statuses []bool
		controls := newPersistentControls(
			store,
			capture.Pause,
			capture.RequestResume,
			func(paused bool) { statuses = append(statuses, paused) },
		)
		for range 2 {
			if err := toggleCapture(capture.IsPaused, controls.Pause, controls.Resume, nil); err != nil {
				t.Fatalf("toggle capture: %v", err)
			}
		}
		if got, want := statuses, []bool{true, false}; !slices.Equal(got, want) {
			t.Fatalf("power button status broadcasts = %v, want %v", got, want)
		}
	})
}
