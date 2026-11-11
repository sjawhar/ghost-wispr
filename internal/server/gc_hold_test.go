package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// A GC pass that held back (it could not check Drive) shows in GET
// /api/status as a warning the web UI's banner shows, with the disk use, and
// as gc_hold; none shows when GC did not hold back.
func TestAPIStatusReportsAGCHold(t *testing.T) {
	since := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		hold    *GCHold
		warning string // "" for none
	}{
		{name: "no hold"},
		{
			name: "held back",
			hold: &GCHold{Reason: "sync to Google Drive is on but not running, so nothing can check that Drive still has a session",
				Since: since, DiskUsedBytes: 12 * 1024 * 1024 * 1024, DiskTotalBytes: 16 * 1024 * 1024 * 1024},
			warning: "Garbage collection is on hold since 2026-11-06 09:30 UTC: sync to Google Drive is on but not running, so nothing can check that Drive still has a session. " +
				"No session is deleted and no audio removed, so the disk fills; the disk is 75% full (12.0 of 16.0 GB). Fix Google Drive sync.",
		},
		{
			name:    "disk use unknown",
			hold:    &GCHold{Reason: "could not ask Drive whether it has the session's folder: timeout", Since: since},
			warning: "Garbage collection is on hold since 2026-11-06 09:30 UTC: could not ask Drive whether it has the session's folder: timeout. No session is deleted and no audio removed, so the disk fills; disk use unknown. Fix Google Drive sync.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
				sessionsByDate: map[string][]storage.Session{},
				sessions:       map[string]storage.Session{},
				segments:       map[string][]transcribe.Segment{},
			}, &ControlHooks{GCHold: func() *GCHold { return tc.hold }}, "", nil)
			if err != nil {
				t.Fatalf("Handler failed: %v", err)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))

			var resp struct {
				Warnings []string `json:"warnings"`
				GCHold   *struct {
					Reason         string `json:"reason"`
					Since          string `json:"since"`
					DiskUsedBytes  int64  `json:"disk_used_bytes"`
					DiskTotalBytes int64  `json:"disk_total_bytes"`
				} `json:"gc_hold"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode status: %v (body=%s)", err, rr.Body.String())
			}
			var want []string
			if tc.warning != "" {
				want = []string{tc.warning}
			}
			if !slices.Equal(resp.Warnings, want) {
				t.Errorf("warnings = %q, want %q", resp.Warnings, want)
			}
			switch {
			case tc.hold == nil && resp.GCHold != nil:
				t.Errorf("gc_hold = %+v, want null", *resp.GCHold)
			case tc.hold != nil && (resp.GCHold == nil || resp.GCHold.Reason != tc.hold.Reason || resp.GCHold.Since != "2026-11-06T09:30:00Z" ||
				resp.GCHold.DiskUsedBytes != tc.hold.DiskUsedBytes || resp.GCHold.DiskTotalBytes != tc.hold.DiskTotalBytes):
				t.Errorf("gc_hold = %+v, want %+v", resp.GCHold, *tc.hold)
			}
		})
	}
}

// The sessions GC keeps until a sync verifies their documents on Drive show
// in GET /api/status as unverified_sessions and, when there are any, as a
// warning the web UI's banner shows.
func TestAPIStatusReportsUnverifiedSessions(t *testing.T) {
	for n, warning := range map[int]string{
		0: "",
		1: "1 older session is kept until its Drive documents are re-checked, the next time it is synced (when its title or summary changes).",
		3: "3 older sessions are kept until their Drive documents are re-checked, each the next time it is synced (when its title or summary changes).",
	} {
		h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
			sessionsByDate: map[string][]storage.Session{},
			sessions:       map[string]storage.Session{},
			segments:       map[string][]transcribe.Segment{},
		}, &ControlHooks{UnverifiedSessions: func() int { return n }}, "", nil)
		if err != nil {
			t.Fatalf("Handler failed: %v", err)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var resp struct {
			Warnings   []string `json:"warnings"`
			Unverified int      `json:"unverified_sessions"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		var want []string
		if warning != "" {
			want = []string{warning}
		}
		if resp.Unverified != n || !slices.Equal(resp.Warnings, want) {
			t.Errorf("%d unverified: unverified_sessions %d, warnings %q; want %d, %q", n, resp.Unverified, resp.Warnings, n, want)
		}
	}
}

// The sessions kept on this unit only (their Drive folder gone or
// unreadable) show in GET /api/status as unit_only_sessions and, when there
// are any, as a warning the web UI's banner shows.
func TestAPIStatusReportsSessionsKeptOnThisUnitOnly(t *testing.T) {
	for n, warning := range map[int]string{
		0: "",
		1: "1 session is kept on this unit only, because its Drive folder is gone or unreadable; retry sync once the folder is back",
		3: "3 sessions are kept on this unit only, because their Drive folder is gone or unreadable; retry sync once the folder is back",
	} {
		h, err := Handler(testStaticFS(t), NewHub(), &apiStoreStub{
			sessionsByDate: map[string][]storage.Session{},
			sessions:       map[string]storage.Session{},
			segments:       map[string][]transcribe.Segment{},
		}, &ControlHooks{UnitOnlySessions: func() int { return n }}, "", nil)
		if err != nil {
			t.Fatalf("Handler failed: %v", err)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var resp struct {
			Warnings []string `json:"warnings"`
			UnitOnly int      `json:"unit_only_sessions"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		var want []string
		if warning != "" {
			want = []string{warning}
		}
		if resp.UnitOnly != n || !slices.Equal(resp.Warnings, want) {
			t.Errorf("%d kept on this unit only: unit_only_sessions %d, warnings %q; want %d, %q", n, resp.UnitOnly, resp.Warnings, n, want)
		}
	}
}
