package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/gdrive"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/stopping"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/storeguard"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// heldUntilStop stands in for every slow dependency of the requests' store
// work (the summarizer, the transcriber, the Drive syncer, a restore from
// Drive, a batch submit and poll). It holds each call until its context
// ends, as the stop cuts it off, or until release closes, which the test
// does once the store is closed. Then it writes the store, as the work that
// called it would: work the stop waited for writes before the store closed;
// work that started outside it writes after, and the guard records that.
type heldUntilStop struct {
	store   *storage.SQLiteStore
	release chan struct{}
	mu      sync.Mutex
	entered map[string]int
}

func (h *heldUntilStop) hold(ctx context.Context, what string) error {
	h.mu.Lock()
	h.entered[what]++
	h.mu.Unlock()
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-h.release:
		err = errors.New("released after the store closed")
	}
	_, _ = h.store.DB().Exec(`UPDATE sessions SET title = title WHERE id = (SELECT id FROM sessions ORDER BY id LIMIT 1)`)
	return err
}

func (h *heldUntilStop) count(what string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.entered[what]
}

func (h *heldUntilStop) Summarize(ctx context.Context, _, _ string) (string, string, string, string, error) {
	return "", "", "", "", h.hold(ctx, "summarize")
}

func (h *heldUntilStop) SummarizeReported(ctx context.Context, _, _ string) (string, string, string, string, string, error) {
	return "", "", "", "", "", h.hold(ctx, "summarize")
}

func (h *heldUntilStop) SummarizeWithPreset(ctx context.Context, _, _, _ string) (string, string, string, error) {
	return "", "", "", h.hold(ctx, "summarize")
}

func (h *heldUntilStop) CanSync(context.Context, string) error { return nil }

func (h *heldUntilStop) SyncSession(ctx context.Context, _ string) error { return h.hold(ctx, "sync") }

func (h *heldUntilStop) FolderOnDrive(context.Context, string) (bool, error) { return true, nil }

func (h *heldUntilStop) RecordingsMatch(context.Context, string, map[string]string) (bool, error) {
	return true, nil
}

// routeRecorder lists each route the server registers, as it registers it
// (ControlHooks.RouteHandler), and which of them a request reached.
type routeRecorder struct {
	mu         sync.Mutex
	registered []string
	reached    map[string]bool
}

func newRouteRecorder() *routeRecorder { return &routeRecorder{reached: map[string]bool{}} }

func (r *routeRecorder) handler(pattern string, handler http.Handler) http.Handler {
	r.mu.Lock()
	r.registered = append(r.registered, pattern)
	r.mu.Unlock()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.reached[pattern] = true
		r.mu.Unlock()
		handler.ServeHTTP(w, req)
	})
}

func (r *routeRecorder) routes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(slices.Values(r.registered))
}

// unreached is each registered route no request reached.
func (r *routeRecorder) unreached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var missed []string
	for _, pattern := range r.registered {
		if !r.reached[pattern] {
			missed = append(missed, pattern)
		}
	}
	return missed
}

// routeRequest is the request the test sends for one route.
type routeRequest struct {
	path string
	body string
	// held marks a request whose own handler runs store work inside the
	// stop's wait (session.Manager.Run) that the held dependency keeps
	// running until the stop: it is sent on its own goroutine.
	held bool
}

// TestNoRequestWritesTheStoreAfterTheStop drives every route the server
// registers, as it registers them (routeRecorder), with the store work main
// wires (wireStoreWork, wireSegmentDeletion) and every slow dependency held
// until the stop (heldUntilStop). It then stops as main does: no more
// requests (stopServing), then the stop's wait (stopRecovery.stop). It fails
// if anything writes the store once that has returned, when main closes the
// store (storeguard records each write), if the server takes a request after
// it stopped serving, or if a registered route's handler was never reached,
// so a new route is driven too.
func TestNoRequestWritesTheStoreAfterTheStop(t *testing.T) {
	dir := t.TempDir()
	store := openResumeStore(t, filepath.Join(dir, "requests.db"))
	held := &heldUntilStop{store: store, release: make(chan struct{}), entered: map[string]int{}}
	at := time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)
	ids := map[string]string{}
	for i, name := range []string{"read", "words", "refine", "sync", "delete", "mergeA", "mergeB", "autoA", "autoB"} {
		id := at.Add(time.Duration(i) * time.Hour).Format("20060102150405")
		switch name {
		case "mergeB": // ten minutes after its pair: past auto-merge's 5 minutes, so only the merge request takes it
			id = at.Add(time.Duration(i-1)*time.Hour + 10*time.Minute).Format("20060102150405")
		case "autoB": // two minutes after its pair: auto-merge takes autoA and autoB
			id = at.Add(time.Duration(i-1)*time.Hour + 2*time.Minute).Format("20060102150405")
		}
		ids[name] = id
		started, _ := time.Parse("20060102150405", id)
		path := filepath.Join(dir, id+".mp3")
		if err := os.WriteFile(path, []byte("synthetic fixture; no room audio"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateSession(id, started); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendSegment(id, transcribe.Segment{Text: "synthetic words, no room audio", StartTime: 0, EndTime: 1, Timestamp: started}); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, started.Add(time.Minute), path, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`UPDATE sessions SET summary_status = ?, refinement_status = ? WHERE id IN (?, ?)`, storage.SummaryFailed, storage.RefinementFailed, ids["words"], ids["refine"]); err != nil {
		t.Fatal(err)
	}
	recovery := recoverAtStart(store)
	cfgStore := appLogConfigStore(t)
	if err := cfgStore.Update(func(c *config.Config) { c.AudioDir = dir }); err != nil {
		t.Fatal(err)
	}

	manager := session.NewManager(store, nil, held, nil, session.NewDetector(time.Hour), 0)
	manager.SetBatchTranscriber(batchFunc(func(ctx context.Context, _ string) (transcribe.BatchResult, error) {
		return transcribe.BatchResult{}, held.hold(ctx, "transcribe")
	}))
	warnings := newWarningsRegistry()
	refused := newRefusedCuts(store)
	deletion, err := newAppDeleteRunner(store, cfgStore, manager, manager.RefineSession, manager.SummarizeSession, refused, warnings, discardLogger)
	if err != nil {
		t.Fatal(err)
	}
	drive := &driveSync{}
	drive.Set(held)
	hooks := &server.ControlHooks{}
	recorder := newRouteRecorder()
	hooks.RouteHandler = recorder.handler
	// The preset routes main enables (review 6004998055 of #132): main's hooks
	// read the store and call a model; these answer at once.
	hooks.TestPreset = func(context.Context, string, string) (string, error) { return "", nil }
	hooks.GeneratePreset = func(context.Context, string) (config.Preset, error) { return config.Preset{}, nil }
	hooks.RefinePreset = func(context.Context, config.Preset, string) (config.Preset, error) { return config.Preset{}, nil }
	wireStoreWork(hooks, &storeWork{
		manager:                       manager,
		store:                         store,
		drive:                         drive,
		deletion:                      deletion,
		summarizer:                    fixedSummarizerOf(held),
		hub:                           &recordingBroadcaster{},
		publishSummaryReady:           func(string) {},
		recordMergedRefinementFailure: newMergedRefinementFailureRecorder(store),
		restoreFromDrive: func(ctx context.Context) (*gdrive.RestoreResult, error) {
			return nil, held.hold(ctx, "restore")
		},
		cfgStore: cfgStore,
		submitSummaryBatch: func(ctx context.Context, _ []string) (string, int, error) {
			return "", 0, held.hold(ctx, "submit")
		},
		pollSummaryBatch: func(ctx context.Context, _ string) (map[string]any, error) {
			return nil, held.hold(ctx, "poll")
		},
	})
	wireSegmentDeletion(hooks, warnings, refused, deletion)
	handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, hooks, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	base := "http://" + listener.Addr().String()
	guard := storeguard.New(t, store)

	special := map[string]routeRequest{
		"DELETE /api/sessions/{id}":                      {path: "/api/sessions/" + ids["delete"]},
		"DELETE /api/sessions/{id}/segments/{segmentID}": {path: "/api/sessions/" + ids["words"] + "/segments/1?mode=text&revision=1"},
		"POST /api/sessions/merge":                       {body: `{"session_ids":["` + ids["mergeA"] + `","` + ids["mergeB"] + `"]}`},
		"POST /api/sessions/{id}/resummarize":            {path: "/api/sessions/" + ids["words"] + "/resummarize", body: `{"preset":"minutes"}`},
		"POST /api/sessions/{id}/retry-summary":          {path: "/api/sessions/" + ids["words"] + "/retry-summary"},
		"POST /api/sessions/{id}/retry-sync":             {path: "/api/sessions/" + ids["sync"] + "/retry-sync"},
		"POST /api/sessions/{id}/retry-refinement":       {path: "/api/sessions/" + ids["refine"] + "/retry-refinement"},
		"PATCH /api/sessions/{id}":                       {path: "/api/sessions/" + ids["read"], body: `{"title":"A title"}`},
		"POST /api/restore/gdrive":                       {held: true},
		"POST /api/sessions/batch-backfill":              {held: true},
		"GET /api/batches/{name...}":                     {path: "/api/batches/batches/held", held: true},
	}
	routes := recorder.routes()
	for _, want := range []string{"DELETE /api/sessions/{id}", "POST /api/sessions/merge", "POST /api/sessions/{id}/retry-refinement"} {
		if !slices.Contains(routes, want) {
			t.Fatalf("control: the routes the server registered lack %q: %v", want, routes)
		}
	}
	if len(routes) < 40 {
		t.Fatalf("control: UNKNOWN: the server registered %d routes, want every one (about 50)", len(routes))
	}
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(method, path, body string) (*http.Response, error) {
		request, err := http.NewRequest(method, base+path, bytes.NewBufferString(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		return client.Do(request)
	}
	var heldRequests sync.WaitGroup
	for _, route := range routes {
		method, pattern, ok := strings.Cut(route, " ")
		if !ok {
			method, pattern = http.MethodGet, route
		}
		r, ok := special[route]
		if !ok {
			r.path = strings.NewReplacer("{id}", ids["read"], "{segmentID}", "1", "{name...}", "batches/read", "{name}", "minutes").Replace(pattern)
		}
		if r.path == "" {
			r.path = pattern
		}
		if r.body == "" && method != http.MethodGet && method != http.MethodDelete {
			r.body = "{}"
		}
		if r.held {
			heldRequests.Add(1)
			go func() {
				defer heldRequests.Done()
				if response, err := (&http.Client{}).Do(mustRequest(t, method, base+r.path, r.body)); err == nil {
					_ = response.Body.Close()
				}
			}()
			continue
		}
		response, err := send(method, r.path, r.body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, r.path, err)
		}
		_ = response.Body.Close()
	}
	for _, what := range []string{"summarize", "transcribe", "sync", "restore", "submit", "poll"} {
		deadline := time.Now().Add(5 * time.Second)
		for held.count(what) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("control: no request's work reached the held %s, so the stop cut nothing off", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopServing(shutdownCtx, httpServer, 200*time.Millisecond)
	recovery.stop(shutdownCtx, manager, lifecyclePublishWait)
	guard.Close(t)
	if response, err := send(http.MethodPost, "/api/sessions/start", "{}"); err == nil {
		_ = response.Body.Close()
		t.Errorf("the server took a request after it stopped serving: %s", response.Status)
	}
	// A request a connection accepted before the stop reaches the handler
	// after it: main's admission refuses it, so its handler writes nothing.
	late := httptest.NewRecorder()
	handler.ServeHTTP(late, mustRequest(t, http.MethodPatch, "/api/sessions/"+ids["read"], `{"title":"after the stop"}`))
	if late.Code != http.StatusServiceUnavailable {
		t.Errorf("a request that reached the handler after the stop = %d %s, want 503: main does not admit requests through the stop's work", late.Code, late.Body.String())
	}
	close(held.release)
	heldRequests.Wait()
	time.Sleep(200 * time.Millisecond) // work the stop left would write now
	guard.Check(t)
	for _, pattern := range recorder.unreached() {
		t.Errorf("no request reached the handler of route %q: drive it, so its store work is checked", pattern)
	}
}

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	return request
}

// TestStopServingClosesARequestThatOutlastsItsWait fails if stopServing waits
// past its wait for a request that does not end (a long download), which
// would use the time the store's work has to end in, or leaves the server
// taking requests.
func TestStopServingClosesARequestThatOutlastsItsWait(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inside := make(chan struct{})
	httpServer := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inside)
		<-r.Context().Done()
	})}
	go func() { _ = httpServer.Serve(listener) }()
	go func() {
		if response, err := http.Get("http://" + listener.Addr().String()); err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-inside:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the request never reached the handler")
	}
	// The stop's own deadline is far off: only the wait may end the wait.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	began := time.Now()
	stopServing(shutdownCtx, httpServer, 100*time.Millisecond)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("stopServing took %v with a 100 ms wait", took)
	}
	if response, err := http.Get("http://" + listener.Addr().String()); err == nil {
		_ = response.Body.Close()
		t.Fatal("the server took a request after stopServing")
	}
}

// heldTitleStore holds UpdateTitle, which the PATCH handler calls itself,
// until release closes, and records what the store answered (wrote, when
// set).
type heldTitleStore struct {
	*storage.SQLiteStore
	entered, release chan struct{}
	wrote            chan error
}

func (s *heldTitleStore) UpdateTitle(id, title string) error {
	close(s.entered)
	<-s.release
	err := s.SQLiteStore.UpdateTitle(id, title)
	if s.wrote != nil {
		s.wrote <- err
	}
	return err
}

// TestTheStopWaitsForAHandlerThatOutlastsTheHTTPWait fails if the stop, as
// main runs it (stopServing, then stopRecovery.stop), returns while an
// admitted request's handler still runs past the HTTP server's wait, so the
// handler writes the store after main closed it (storeguard records it): the
// server's Close closes the connection but does not end its handler. The
// handler is the PATCH of a title, which writes the store itself. From review
// 6004725672 of #132; the control is a handler that ends within the wait.
func TestTheStopWaitsForAHandlerThatOutlastsTheHTTPWait(t *testing.T) {
	for _, past := range []bool{false, true} {
		name := "control: the handler ends within the HTTP wait"
		if past {
			name = "the handler outlasts the HTTP wait"
		}
		t.Run(name, func(t *testing.T) {
			db := openResumeStore(t, filepath.Join(t.TempDir(), "http.db"))
			const id = "20261120090000"
			if err := db.CreateSession(id, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := db.EndSession(id, time.Now(), "", ""); err != nil {
				t.Fatal(err)
			}
			store := &heldTitleStore{SQLiteStore: db, entered: make(chan struct{}), release: make(chan struct{})}
			guard := storeguard.New(t, db)
			manager := session.NewManager(db, nil, nil, nil, session.NewDetector(time.Hour), 0)
			handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{AdmitRequest: admitRequestsThrough(manager)}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
			go func() { _ = httpServer.Serve(listener) }()
			go func() {
				request := mustRequest(t, http.MethodPatch, "http://"+listener.Addr().String()+"/api/sessions/"+id, `{"title":"set while the app stops"}`)
				if response, err := http.DefaultClient.Do(request); err == nil {
					_ = response.Body.Close()
				}
			}()
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("control: the PATCH handler did not reach UpdateTitle")
			}
			const httpWait = 100 * time.Millisecond
			hold := 10 * time.Millisecond
			if past {
				hold = 3 * httpWait
			}
			released := make(chan time.Time, 1)
			time.AfterFunc(hold, func() {
				released <- time.Now()
				close(store.release)
			})
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stopServing(shutdownCtx, httpServer, httpWait)
			recoverAtStart(db).stop(shutdownCtx, manager, lifecyclePublishWait)
			stopped := time.Now()
			if shutdownCtx.Err() != nil {
				t.Fatal("control: the stop's shared deadline ran out")
			}
			guard.Close(t)
			select {
			case at := <-released:
				if stopped.Before(at) {
					t.Errorf("the stop returned %v before the handler it admitted was released", at.Sub(stopped))
				}
			default:
				t.Errorf("the stop returned before the handler it admitted was released (%v after the request)", hold)
			}
			time.Sleep(50 * time.Millisecond)
			guard.Check(t)
			if got, err := db.GetSession(id); err != nil || got.Title != "set while the app stops" {
				t.Errorf("control: the handler's write did not land before the stop returned: %q, %v", got.Title, err)
			}
		})
	}
}

// TestAHandlerPastTheShutdownsDeadlineNeverWritesTheClosedStore fails if a
// request's handler, admitted before the stop and held past the shutdown's
// whole deadline, writes the store once main has closed it (closeStore): the
// stop gives up waiting at the deadline, and the store refuses the handler's
// late write itself (stopping.ErrStopped), so the title stays as it was. From
// review 6004998055 of #132.
func TestAHandlerPastTheShutdownsDeadlineNeverWritesTheClosedStore(t *testing.T) {
	runParallel(t)
	path := filepath.Join(t.TempDir(), "deadline.db")
	db := openResumeStore(t, path)
	const id = "20261120090000"
	if err := db.CreateSession(id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(id, time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	store := &heldTitleStore{SQLiteStore: db, entered: make(chan struct{}), release: make(chan struct{}), wrote: make(chan error, 1)}
	manager := session.NewManager(db, nil, nil, nil, session.NewDetector(time.Hour), 0)
	handler, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), store, &server.ControlHooks{AdmitRequest: admitRequestsThrough(manager)}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	go func() {
		request := mustRequest(t, http.MethodPatch, "http://"+listener.Addr().String()+"/api/sessions/"+id, `{"title":"set past the deadline"}`)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("control: the PATCH handler did not reach UpdateTitle")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	stopServing(shutdownCtx, httpServer, 100*time.Millisecond)
	recoverAtStart(db).stop(shutdownCtx, manager, lifecyclePublishWait)
	if shutdownCtx.Err() == nil {
		t.Fatal("control: the stop returned before the shutdown's deadline, with the handler held")
	}
	if !closeStore(shutdownCtx, db) {
		t.Fatal("closeStore left the store open, though no statement ran on it")
	}
	close(store.release)
	select {
	case err := <-store.wrote:
		if !errors.Is(err, stopping.ErrStopped) {
			t.Errorf("the handler's write after the store closed = %v, want stopping.ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not end once released")
	}
	reopened := openResumeStore(t, path)
	if got, err := reopened.GetSession(id); err != nil || got.Title != "" {
		t.Errorf("the title after the late write was refused = %q, %v; want none", got.Title, err)
	}
}
