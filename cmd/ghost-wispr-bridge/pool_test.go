package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	runapi "google.golang.org/api/run/v2"
)

// fakePoolName is the worker pool every watcher test sets the count of.
const fakePoolName = "projects/example-project/locations/us-central1/workerPools/call-pool"

// fakePoolActAs is the answer Cloud Run gives a patch whose caller may not act
// as the pool's service account: Google's error form (AIP-193), with the
// permission in both the message and the ErrorInfo's metadata.
const fakePoolActAs = `{"error":{"code":403,` +
	`"message":"Permission 'iam.serviceAccounts.actAs' denied on service account call@example-project.iam.gserviceaccount.com (or it may not exist).",` +
	`"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED",` +
	`"domain":"iam.googleapis.com","metadata":{"permission":"iam.serviceAccounts.actAs"}}]}}`

// fakePoolOpActAs is the same refusal as the error of a count change's
// long-running operation: a google.rpc.Status, whose code is gRPC's
// PERMISSION_DENIED (7), not an HTTP status.
const fakePoolOpActAs = `{"code":7,` +
	`"message":"Permission 'iam.serviceAccounts.actAs' denied on service account call@example-project.iam.gserviceaccount.com (or it may not exist).",` +
	`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"IAM_PERMISSION_DENIED",` +
	`"domain":"iam.googleapis.com","metadata":{"permission":"iam.serviceAccounts.actAs"}}]}`

// fakePoolOpPrefix is the resource name of each count change's operation,
// before its number.
const fakePoolOpPrefix = "projects/example-project/locations/us-central1/operations/op-"

// fakePoolServer is Cloud Run's Admin API v2 for one worker pool, over HTTP:
// a get answers the pool with its scaling (a count of 0 left out, as proto3's
// JSON leaves out a zero), and a patch starts a long-running operation that
// sets the count by its update mask (AIP-134: a field the mask names and the
// body leaves out is cleared). Cloud Run keeps the old count while the
// operation is pending: the operation ends when it is read, unless the fake
// holds it, and ends with an error when the fake fails it.
type fakePoolServer struct {
	t        *testing.T
	mu       sync.Mutex
	count    int64
	refuse   string // when set, the body of a 403 that answers each patch
	getFails bool   // when set, each get answers 503
	// readBackFails, when set, answers 503 to each get after the first patch.
	readBackFails bool
	hold          bool   // when set, each operation stays pending until release
	opFails       string // when set, the google.rpc.Status each new operation ends with
	opRefuse      string // when set, the body of a 403 that answers each read of an operation
	gets          int
	opReads       int
	patches       []fakePoolPatch
	ops           []*fakePoolOp
}

// fakePoolOp is the long-running operation of one count change.
type fakePoolOp struct {
	count int64
	fails string // the google.rpc.Status it ends with; "" ends it with the new count
	done  bool
}

// fakePoolPatch is one patch the fake took: its query and its body as sent.
type fakePoolPatch struct {
	query url.Values
	body  string
}

// newFakePoolServer starts the fake with the pool's count at count, and gives
// it with the poolAPI the watcher uses, pointed at it. A count change reads
// its operation every millisecond and waits for it at most fakePoolWait.
func newFakePoolServer(t *testing.T, count int64) (*fakePoolServer, poolAPI) {
	t.Helper()
	f, opts := startFakePoolServer(t, count)
	svc, err := runapi.NewService(context.Background(), opts...)
	if err != nil {
		t.Fatalf("run service: %v", err)
	}
	pool := newRunPool(svc, fakePoolName)
	pool.wait, pool.poll = fakePoolWait, time.Millisecond
	return f, pool
}

// fakePoolWait is a count change's bound in the watcher's tests: the time a
// test with a pending operation waits, in real time.
const fakePoolWait = 500 * time.Millisecond

// startFakePoolServer starts the fake with the pool's count at count, and
// gives it with the client options that reach it.
func startFakePoolServer(t *testing.T, count int64) (*fakePoolServer, []option.ClientOption) {
	t.Helper()
	f := &fakePoolServer{t: t, count: count}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, []option.ClientOption{option.WithEndpoint(srv.URL + "/"), option.WithHTTPClient(srv.Client())}
}

func (f *fakePoolServer) set(fn func(f *fakePoolServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakePoolServer) patchList() []fakePoolPatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePoolPatch(nil), f.patches...)
}

// release ends each operation the fake holds, as Cloud Run ends them, and
// lets each later one end when it is read.
func (f *fakePoolServer) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = false
	for _, op := range f.ops {
		f.end(op)
	}
}

// end ends op: with its error, or with the pool at its count. f.mu is held.
func (f *fakePoolServer) end(op *fakePoolOp) {
	if op.done {
		return
	}
	op.done = true
	if op.fails == "" {
		f.count = op.count
	}
}

// fakePoolOpJSON is the operation numbered n as Cloud Run answers it.
func fakePoolOpJSON(n int, op *fakePoolOp) string {
	name := fakePoolOpPrefix + strconv.Itoa(n)
	switch {
	case !op.done:
		return fmt.Sprintf(`{"name":%q,"done":false}`, name)
	case op.fails != "":
		return fmt.Sprintf(`{"name":%q,"done":true,"error":%s}`, name, op.fails)
	default:
		return fmt.Sprintf(`{"name":%q,"done":true,"response":{"name":%q}}`, name, fakePoolName)
	}
}

func (f *fakePoolServer) answer(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func (f *fakePoolServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := strings.CutPrefix(r.URL.Path, "/v2/"+fakePoolOpPrefix); ok && r.Method == http.MethodGet {
		f.opReads++
		i, err := strconv.Atoi(n)
		switch {
		case f.opRefuse != "":
			f.answer(w, http.StatusForbidden, f.opRefuse)
		case err != nil || i < 1 || i > len(f.ops):
			f.answer(w, http.StatusNotFound, `{"error":{"code":404,"message":"no such operation","status":"NOT_FOUND"}}`)
		default:
			if !f.hold {
				f.end(f.ops[i-1])
			}
			f.answer(w, http.StatusOK, fakePoolOpJSON(i, f.ops[i-1]))
		}
		return
	}
	if r.URL.Path != "/v2/"+fakePoolName {
		f.answer(w, http.StatusNotFound, `{"error":{"code":404,"message":"no such worker pool","status":"NOT_FOUND"}}`)
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.gets++
		if f.getFails || (f.readBackFails && len(f.patches) > 0) {
			f.answer(w, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"The service is currently unavailable.","status":"UNAVAILABLE"}}`)
			return
		}
		scaling := map[string]any{}
		if f.count != 0 {
			scaling["manualInstanceCount"] = f.count
		}
		b, _ := json.Marshal(map[string]any{"name": fakePoolName, "scaling": scaling})
		f.answer(w, http.StatusOK, string(b))
	case http.MethodPatch:
		body, _ := io.ReadAll(r.Body)
		f.patches = append(f.patches, fakePoolPatch{query: r.URL.Query(), body: string(body)})
		if f.refuse != "" {
			f.answer(w, http.StatusForbidden, f.refuse)
			return
		}
		if r.URL.Query().Get("updateMask") != "scaling.manualInstanceCount" {
			f.answer(w, http.StatusBadRequest, `{"error":{"code":400,"message":"update mask","status":"INVALID_ARGUMENT"}}`)
			return
		}
		var pool struct {
			Scaling struct {
				ManualInstanceCount int64 `json:"manualInstanceCount"`
			} `json:"scaling"`
		}
		if err := json.Unmarshal(body, &pool); err != nil {
			f.answer(w, http.StatusBadRequest, `{"error":{"code":400,"message":"bad body","status":"INVALID_ARGUMENT"}}`)
			return
		}
		f.ops = append(f.ops, &fakePoolOp{count: pool.Scaling.ManualInstanceCount, fails: f.opFails})
		f.answer(w, http.StatusOK, fakePoolOpJSON(len(f.ops), f.ops[len(f.ops)-1]))
	}
}

func TestThePoolReadsTheDeniedPermissionFromGooglesError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		permission string
	}{
		{"the error's details name it", fakePoolActAs, 403, "iam.serviceAccounts.actAs"},
		{"only the message names it",
			`{"error":{"code":403,"message":"Permission 'run.workerpools.update' denied on resource (or it may not exist).","status":"PERMISSION_DENIED"}}`,
			403, "run.workerpools.update"},
		{"nothing names it", `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`, 403, ""},
		{"not a permission", `{"error":{"code":409,"message":"Conflict","status":"ABORTED"}}`, 409, ""},
		// A permission is a name of Google's form, never other text an
		// answer's message or detail holds, so a log line can carry it.
		{"the message names no permission's form",
			`{"error":{"code":403,"message":"Permission '` + watchSentinel + ` of a meeting' denied on resource.","status":"PERMISSION_DENIED"}}`,
			403, ""},
		{"the details name no permission's form",
			`{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
				`"metadata":{"permission":"` + watchSentinel + ` of a meeting"}}]}}`,
			403, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pool := newFakePoolServer(t, 0)
			f.set(func(f *fakePoolServer) { f.refuse = tc.body })
			err := pool.SetCount(context.Background(), 1)
			var perr *poolError
			if !errors.As(err, &perr) {
				t.Fatalf("SetCount gave %v, want a poolError", err)
			}
			if perr.Status != tc.status || perr.Permission != tc.permission {
				t.Errorf("poolError status %d permission %q, want %d %q", perr.Status, perr.Permission, tc.status, tc.permission)
			}
		})
	}
}

func TestThePoolCountIsReadFromTheWorkerPool(t *testing.T) {
	for _, want := range []int64{0, 1} {
		_, pool := newFakePoolServer(t, want)
		got, err := pool.Count(context.Background())
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if got != want {
			t.Errorf("Count = %d, want %d", got, want)
		}
	}
	f, pool := newFakePoolServer(t, 1)
	f.set(func(f *fakePoolServer) { f.getFails = true })
	_, err := pool.Count(context.Background())
	var perr *poolError
	if !errors.As(err, &perr) || perr.Status != 503 {
		t.Errorf("Count with the API down gave %v, want a poolError of status 503", err)
	}
}

// A count change is done only when Cloud Run's long-running operation ends
// without an error: SetCount waits for it within its bound, and gives the
// operation's own failure, or a refused read of it, as a poolError with its
// HTTP status and permission. It fails if SetCount returns at the patch, or
// takes a failed, pending or unread operation for a change made.
func TestACountChangeWaitsForItsOperation(t *testing.T) {
	const quota = `{"code":8,"message":"Quota exceeded for instances in region us-central1."}`
	const opsDenied = `{"error":{"code":403,"message":"Permission 'run.operations.get' denied on resource (or it may not exist).","status":"PERMISSION_DENIED"}}`
	for _, tc := range []struct {
		name       string
		setup      func(f *fakePoolServer)
		status     int // a poolError's; 0: no poolError
		permission string
		pending    bool  // SetCount ends at its bound with the change unconfirmed
		count      int64 // the count Cloud Run reports after SetCount
	}{
		{"the operation ends", func(*fakePoolServer) {}, 0, "", false, 1},
		{"the operation fails for a permission", func(f *fakePoolServer) { f.opFails = fakePoolOpActAs }, 403, "iam.serviceAccounts.actAs", false, 0},
		{"the operation fails for quota", func(f *fakePoolServer) { f.opFails = quota }, 429, "", false, 0},
		{"the operation cannot be read", func(f *fakePoolServer) { f.opRefuse = opsDenied }, 403, "run.operations.get", false, 0},
		{"the operation is pending at the bound", func(f *fakePoolServer) { f.hold = true }, 0, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pool := newFakePoolServer(t, 0)
			f.set(tc.setup)
			err := pool.SetCount(context.Background(), 1)
			var perr *poolError
			switch {
			case tc.pending:
				if err == nil || errors.As(err, &perr) || answerStatus(err) != "none" {
					t.Errorf("SetCount with its operation pending = %v; want an error with no HTTP status (status=none)", err)
				}
			case tc.status == 0:
				if err != nil {
					t.Errorf("SetCount = %v; want nil once the operation ended", err)
				}
			case !errors.As(err, &perr):
				t.Errorf("SetCount = %v; want a poolError of status %d", err, tc.status)
			case perr.Status != tc.status || perr.Permission != tc.permission:
				t.Errorf("poolError status %d permission %q, want %d %q", perr.Status, perr.Permission, tc.status, tc.permission)
			}
			var reads int
			f.set(func(f *fakePoolServer) { reads = f.opReads })
			if reads == 0 {
				t.Errorf("SetCount never read its operation")
			}
			if got, err := pool.Count(context.Background()); err != nil || got != tc.count {
				t.Errorf("after SetCount Cloud Run reports %d (err %v), want %d", got, err, tc.count)
			}
		})
	}
}
