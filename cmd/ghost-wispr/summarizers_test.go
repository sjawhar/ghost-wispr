package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/session"
	"github.com/sjawhar/ghost-wispr/internal/summary"
)

// chatAPI is a fake OpenAI chat completions API whose summary names the
// model it was asked for.
type chatAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	models []string
}

func startChatAPI(t *testing.T) *chatAPI {
	t.Helper()
	f := &chatAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.models = append(f.models, body.Model)
		f.mu.Unlock()
		content, _ := json.Marshal(map[string]string{"title": "Bridge talk", "summary": "Summary by " + body.Model})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(content)}}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// take returns the models asked for since the last take.
func (f *chatAPI) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	models := f.models
	f.models = nil
	return models
}

// summarizerSlot is the session manager's summarizer setting.
type summarizerSlot struct {
	mu   sync.Mutex
	got  session.Summarizer
	sets int
}

func (m *summarizerSlot) SetSummarizer(s session.Summarizer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got, m.sets = s, m.sets+1
}

func (m *summarizerSlot) current() (session.Summarizer, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.got, m.sets
}

// TestEverySummaryPathFollowsTheSettingsSavedLast fails if, after a settings
// change of summarization.model and its presets through the config store,
// the session manager's summarizer, a resummarize, the presets list, a
// summary batch or a merged session's summary keeps the summarizer built at
// start; if a summarizer a change builds reports no result to the LLM health
// tracker (OnResult); or if any of them still summarizes once summaries are
// off.
func TestEverySummaryPathFollowsTheSettingsSavedLast(t *testing.T) {
	runParallel(t)
	const keyEnv = "EXAMPLE_SUMMARY_KEY"
	api := startChatAPI(t)
	cfgStore, _, err := config.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	preset := func(prompt string) map[string]config.Preset {
		return map[string]config.Preset{"default": {Description: prompt, SystemPrompt: prompt, UserTemplate: "{{transcript}}"}}
	}
	if err := cfgStore.Update(func(c *config.Config) {
		c.Providers = map[string]config.Provider{
			"llm": {API: config.APIOpenAI, URL: api.srv.URL + "/v1", Auth: config.ProviderAuth{Kind: config.AuthAPIKey, Env: keyEnv}},
		}
		c.ProviderKeys = map[string]string{keyEnv: "sk-1"}
		c.Summarization = config.Summarization{Model: "llm/model-a", MinSummaryWords: 1, Presets: preset("Prompt A")}
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	results := 0
	onResult := func(error) {
		mu.Lock()
		results++
		mu.Unlock()
	}
	resultCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return results
	}
	cfg := cfgStore.Get()
	summaries := newSummarizers(summaryClients(cfgStore), onResult)
	manager := &summarizerSlot{}
	manager.SetSummarizer(summaries.apply(&cfg))
	summaries.follow(cfgStore, manager)
	store, _, started := newDeletionTestStore(t, "recording.mp3")
	hub := &recordingBroadcaster{}
	resummarize := resummarizeUntilStop(stopTestManager(t, store), store, summaries.summarizer, hub, func(string) {})
	ctx := t.Context()
	fallbacks := 0
	fallback := func(context.Context, string, time.Time) error { fallbacks++; return nil }
	batchJob := &genai.BatchJob{Dest: &genai.BatchJobDestination{InlinedResponses: []*genai.InlinedResponse{{
		Metadata: map[string]string{"session_id": "session"},
		Response: &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(`{"title":"Bridge talk","summary":"Batch summary"}`, genai.RoleModel)}}},
	}}}}

	// each runs every summary path and reports the models it asked for, the
	// preset prompt each saw, and what the session's summary became.
	each := func(when, model, prompt string) {
		t.Helper()
		api.take()
		got, _ := manager.current()
		reporter, ok := got.(session.ReportedSummarizer)
		if !ok {
			t.Fatalf("%s: the session manager holds %T, want a summarizer", when, got)
		}
		before := resultCount()
		if _, _, _, _, _, err := reporter.SummarizeReported(ctx, "session", "before bridge after bridge"); err != nil {
			t.Errorf("%s: the manager's summarizer: %v", when, err)
		}
		if asked := api.take(); len(asked) != 1 || asked[0] != model {
			t.Errorf("%s: the manager's summarizer asked for models %q, want %s once", when, asked, model)
		}
		if resultCount() == before {
			t.Errorf("%s: the manager's summarizer reported no result to the LLM health tracker", when)
		}
		if err := resummarize(ctx, "session", ""); err != nil {
			t.Errorf("%s: resummarize: %v", when, err)
		}
		if asked := api.take(); len(asked) != 1 || asked[0] != model {
			t.Errorf("%s: a resummarize asked for models %q, want %s once", when, asked, model)
		}
		if sess, _ := store.GetSession("session"); sess.Summary != "Summary by "+model {
			t.Errorf("%s: after a resummarize the summary is %q, want one by %s", when, sess.Summary, model)
		}
		if err := summarizeMerged(ctx, store, summaries.summarizer, hub, func(string) {}, "session", started, fallback); err != nil {
			t.Errorf("%s: merged session: %v", when, err)
		}
		if asked := api.take(); len(asked) != 1 || asked[0] != model {
			t.Errorf("%s: a merged session's summary asked for models %q, want %s once", when, asked, model)
		}
		if got := summaries.presets()["default"].Description; got != prompt {
			t.Errorf("%s: the presets list has %q, want %q", when, got, prompt)
		}
		requests, err := summaries.batchRequests(ctx, []summary.BatchSession{{ID: "session", Transcript: "before bridge after bridge"}})
		if err != nil || len(requests) != 1 || !strings.Contains(fmt.Sprint(requests[0].Config.SystemInstruction.Parts[0].Text), prompt) {
			t.Errorf("%s: a summary batch's requests %v, %v; want one with %q", when, requests, err, prompt)
		}
		if batch := summaries.batchResults(batchJob, []summary.BatchSession{{ID: "session"}}); len(batch) != 1 || batch[0].Error != nil || batch[0].Summary != "Batch summary" {
			t.Errorf("%s: a finished batch's results %+v; want its summary", when, batch)
		}
	}

	each("at start", "model-a", "Prompt A")
	if err := cfgStore.Update(func(c *config.Config) {
		c.Summarization.Model, c.Summarization.Presets = "llm/model-b", preset("Prompt B")
	}); err != nil {
		t.Fatal(err)
	}
	each("after the settings changed the model and presets", "model-b", "Prompt B")

	if err := cfgStore.Update(func(c *config.Config) { c.Summarization.Model = "" }); err != nil {
		t.Fatal(err)
	}
	api.take()
	if got, sets := manager.current(); got != nil || sets != 3 {
		t.Errorf("summaries off: the session manager holds %v after %d settings, want none after 3", got, sets)
	}
	if err := resummarize(ctx, "session", ""); !errors.Is(err, errSummariesOff) {
		t.Errorf("summaries off: resummarize = %v, want %v", err, errSummariesOff)
	}
	if err := summarizeMerged(ctx, store, summaries.summarizer, hub, func(string) {}, "session", started, fallback); err != nil || fallbacks != 1 {
		t.Errorf("summaries off: a merged session = %v with %d fallbacks, want the manager's fallback once", err, fallbacks)
	}
	if got := summaries.presets(); got != nil {
		t.Errorf("summaries off: presets %v, want none", got)
	}
	if _, err := summaries.batchRequests(ctx, []summary.BatchSession{{ID: "session", Transcript: "words"}}); !errors.Is(err, errSummariesOff) {
		t.Errorf("summaries off: a summary batch = %v, want %v", err, errSummariesOff)
	}
	if batch := summaries.batchResults(batchJob, []summary.BatchSession{{ID: "session"}}); len(batch) != 1 || batch[0].SessionID != "session" || batch[0].Summary != "" || !errors.Is(batch[0].Error, errSummariesOff) {
		t.Errorf("summaries off: a finished batch's results %+v; want the session failed because summaries are off", batch)
	}
	if asked := api.take(); len(asked) != 0 {
		t.Errorf("summaries off: the provider was asked for %q, want nothing", asked)
	}
}
