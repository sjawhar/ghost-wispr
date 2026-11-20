package summary

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// liveSegments are a made-up meeting of two speakers, each named in it:
// speaker 0 is Ben, speaker 1 is Dana.
var liveSegments = []transcribe.Segment{
	{Speaker: 0, Text: "Morning. Ben here. Quick one about the warehouse robot launch."},
	{Speaker: 1, Text: "Hi Ben, it's Dana. QA found two bugs in the docking routine yesterday."},
	{Speaker: 0, Text: "Then we move the launch from Thursday to Friday and give QA one more day."},
	{Speaker: 1, Text: "Agreed. I'll tell the floor team and update the schedule today."},
	{Speaker: 0, Text: "Good. Risk is the charger firmware; if it slips we hold the launch a week."},
}

// liveTranscript is liveSegments as every summary path gives them to the
// summarizer: stored as a session's live segments, then read back by
// storage's CanonicalizeForSummary, each after its speaker's label.
func liveTranscript(t *testing.T) string {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	started := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := store.CreateSession("live", started); err != nil {
		t.Fatal(err)
	}
	for i, seg := range liveSegments {
		seg.StartTime, seg.EndTime, seg.Timestamp = float64(5*i), float64(5*i+4), started.Add(time.Duration(5*i)*time.Second)
		if err := store.AppendSegment("live", seg); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EndSession("live", started.Add(time.Minute), "", ""); err != nil {
		t.Fatal(err)
	}
	_, labeled, _, err := store.CanonicalizeForSummary("live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(labeled, "Speaker 0: Morning. Ben here.") || !strings.Contains(labeled, "\nSpeaker 1: Hi Ben, it's Dana.") {
		t.Fatalf("the transcript a summary reads is not labelled by speaker:\n%s", labeled)
	}
	return labeled
}

// TestLiveASummaryNamesBothSpeakers fails if a summary on a real provider,
// for each provider/model in GHOST_WISPR_LIVE_SUMMARY_MODELS (a provider of
// GHOST_WISPR_LIVE_SUMMARY_CONFIG, else of the shipped config,
// deploy/pi/files/ghost-wispr.yaml), is refused, does not read as a title and
// a summary, names either speaker of liveSegments wrongly or not at all, or
// records no model. It goes through the summarizer as the unit does (its
// schema, call timeout, retries and reading), and signs in as the config
// does but as this machine: a Google ID token of its own for a gateway, its
// Application Default Credentials for Vertex AI. It runs only when
// GHOST_WISPR_LIVE_SUMMARY=1:
//
//	GHOST_WISPR_LIVE_SUMMARY=1 GHOST_WISPR_LIVE_SUMMARY_MODELS=vertex/gemini-4-carbon go test -run TestLiveASummaryNamesBothSpeakers -v ./internal/summary
//
// A model gateway's models, such as gateway/openai/gpt-6.1-terra, need a
// config that names the gateway in GHOST_WISPR_LIVE_SUMMARY_CONFIG: the
// shipped config keeps its gateway entry commented out until the gateway
// admits the Pi.
func TestLiveASummaryNamesBothSpeakers(t *testing.T) {
	if os.Getenv("GHOST_WISPR_LIVE_SUMMARY") != "1" {
		t.Skip("set GHOST_WISPR_LIVE_SUMMARY=1 and GHOST_WISPR_LIVE_SUMMARY_MODELS to summarize on real providers")
	}
	path := os.Getenv("GHOST_WISPR_LIVE_SUMMARY_CONFIG")
	if path == "" {
		path = filepath.Join("..", "..", "deploy", "pi", "files", "ghost-wispr.yaml")
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	transcript := liveTranscript(t)
	t.Logf("the summarizer reads:\n%s", transcript)
	refs := strings.Split(os.Getenv("GHOST_WISPR_LIVE_SUMMARY_MODELS"), ",")
	if refs[0] == "" {
		t.Fatal("set GHOST_WISPR_LIVE_SUMMARY_MODELS to the provider/model refs to summarize with, comma-separated")
	}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			name, model, err := config.ParseModelRef(ref)
			if err != nil {
				t.Fatal(err)
			}
			entry, ok := cfg.Providers[name]
			if !ok {
				t.Fatalf("%s names no provider %q", path, name)
			}
			// This machine's own Google identity, not the Pi's credential file.
			p, err := providers.New(&config.Config{Providers: map[string]config.Provider{name: entry}}, providers.Options{}).Get(name)
			if err != nil {
				t.Fatal(err)
			}
			client, err := llm.NewClient(p, model)
			if err != nil {
				t.Fatal(err)
			}
			s := New(config.Summarization{Model: ref, Presets: map[string]config.Preset{"default": {UserTemplate: "{{transcript}}"}}},
				func(_, _ string) (llm.Client, error) { return client, nil })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			started := time.Now()
			result, err := s.SummarizeWithPresetResult(ctx, "live", transcript, "default")
			if err != nil {
				t.Fatalf("summary: %v", err)
			}
			var speakers map[string]speakerNameMetadata
			if err := json.Unmarshal([]byte(result.SpeakerNames), &speakers); err != nil {
				t.Fatalf("speaker names %s: %v", result.SpeakerNames, err)
			}
			t.Logf("%s: %s; title %q, %d summary bytes, speakers %s, model %q",
				ref, time.Since(started).Round(time.Millisecond), result.Title, len(result.Summary), result.SpeakerNames, result.Model)
			if !meaningfulText(result.Title) || !meaningfulText(result.Summary) {
				t.Errorf("title %q, %d summary bytes; want a title and a summary", result.Title, len(result.Summary))
			}
			if !strings.Contains(speakers["0"].Name, "Ben") || !strings.Contains(speakers["1"].Name, "Dana") {
				t.Errorf("speakers %s; want speaker 0 Ben and speaker 1 Dana, whom the transcript names", result.SpeakerNames)
			}
			if result.Model == "" || result.Model == llm.ModelNotReportedByProvider {
				t.Errorf("summary model %q; want the model the provider's answer names", result.Model)
			}
		})
	}
}
