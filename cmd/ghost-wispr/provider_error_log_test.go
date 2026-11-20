package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/server"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/summary"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// TestAProviderErrorPutsNoRoomWordInTheLog: a provider's error can quote the
// request, a transcript, so no log record, no line of GET /api/logs and no
// stored summary error may hold it. Each summary provider answers 400 with an
// error that quotes the request body; the preset router and a session's
// summary each fail through the production clients, logged by the app's
// logger as main builds it. The log must hold both failures with the
// provider's status and code as fields, and nothing hold the planted word.
func TestAProviderErrorPutsNoRoomWordInTheLog(t *testing.T) {
	const planted = "quokkaplantedroomword"
	for _, tc := range []struct {
		provider, code string
		body           func(request string) string
	}{
		{"openai", "bad_content", func(request string) string {
			return fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error","code":"bad_content"}}`, "Invalid content: "+request)
		}},
		{"anthropic", "invalid_request_error", func(request string) string {
			return fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, "Invalid content: "+request)
		}},
		{"gemini", "INVALID_ARGUMENT", func(request string) string {
			return fmt.Sprintf(`{"error":{"code":400,"message":%q,"status":"INVALID_ARGUMENT"}}`, "Invalid content: "+request)
		}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			keepDefaultLogger(t)
			var stdout bytes.Buffer
			_, sink := startAppLogger(appLogConfigStore(t), &stdout) // the app's logger, as main builds it
			h, err := server.Handler(fstest.MapFS{"index.html": {Data: []byte("ok")}}, server.NewHub(), nil, &server.ControlHooks{GetLogs: sink.GetLogs}, "", nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			quoted := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, _ := io.ReadAll(r.Body)
				if strings.Contains(string(request), planted) {
					quoted++
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body(string(request)))
			}))
			t.Cleanup(provider.Close)
			baseURL := provider.URL
			if tc.provider == "openai" {
				baseURL += "/v1"
			}
			factory := func(name, model string) (llm.Client, error) {
				return llm.NewClient(name, "test-key", model, llm.WithBaseURL(baseURL))
			}
			cfg := config.Summarization{
				Model: tc.provider + "/test-model",
				Presets: map[string]config.Preset{
					"default":  {Description: "Any meeting", SystemPrompt: "Summarize.", UserTemplate: "{{transcript}}"},
					"planning": {Description: "A planning meeting", SystemPrompt: "Summarize the plan.", UserTemplate: "{{transcript}}"},
				},
			}
			transcript := strings.Repeat("We agreed on "+planted+" for the launch. ", 40) // past the summary's fewest words
			if _, err := summary.NewRouter(cfg, factory).SelectPreset(context.Background(), transcript); err != nil {
				t.Fatalf("SelectPreset: %v", err)
			}
			store := summaryStore(t, transcript)
			summarizeBackfillSession(context.Background(), store, summary.New(cfg, factory), func(string) {}, summarySessionID)
			if quoted < 2 {
				t.Fatalf("the provider quoted the planted word in %d answers, want both the router's and the summary's", quoted)
			}

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/logs?limit=1000", nil))
			var entries []logging.LogEntry
			if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
				t.Fatalf("decode /api/logs: %v", err)
			}
			logged := map[string]string{}
			for _, entry := range entries {
				logged[entry.Message] = entry.Raw
				if strings.Contains(entry.Raw, planted) || strings.Contains(entry.Raw, "Invalid content") {
					t.Errorf("/api/logs holds the provider's message: %s", entry.Raw)
				}
			}
			for _, want := range []string{"router: falling back to default preset", "summarization failed"} {
				raw, ok := logged[want]
				if !ok {
					t.Errorf("/api/logs holds no record %q, so the check read nothing of it; it holds %v", want, logged)
					continue
				}
				if !strings.Contains(raw, `"provider":"`+tc.provider+`"`) || !strings.Contains(raw, `"status":400`) || !strings.Contains(raw, `"code":"`+tc.code+`"`) {
					t.Errorf("record %q does not name the provider, status 400 and code %s as fields: %s", want, tc.code, raw)
				}
			}
			if strings.Contains(stdout.String(), planted) {
				t.Errorf("the journal holds the planted word:\n%s", stdout.String())
			}
			session, err := store.GetSession(summarySessionID)
			if err != nil {
				t.Fatal(err)
			}
			if session.SummaryError == "" || strings.Contains(session.SummaryError, planted) || strings.Contains(session.SummaryError, "Invalid content") {
				t.Errorf("the session's summary error %q is empty or quotes the provider's message", session.SummaryError)
			}
		})
	}
}

const summarySessionID = "20261120090000"

// summaryStore is a store with one ended session, summarySessionID, whose
// transcript is text.
func summaryStore(t *testing.T, text string) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession(summarySessionID, started); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendSegment(summarySessionID, transcribe.Segment{Text: text, StartTime: 0, EndTime: 3, Timestamp: started}); err != nil {
		t.Fatal(err)
	}
	if err := store.EndSession(summarySessionID, started.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	return store
}
