package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// keepTheDefaultLogger puts slog's default logger, and the log package's
// output and flags, back as they were when the test ends.
func keepTheDefaultLogger(t *testing.T) {
	t.Helper()
	logger, writer, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(logger)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})
}

// A line a provider's SDK writes with the log package can quote what the
// provider answered (genai logs a stream's read error whole), so the app's
// logger writes it as SDKLineWithheld and the SDK's path; a line of the
// app's own is written as it is. genai's Text logs a warning when a response
// has more than one candidate.
func TestALineAProviderSDKLogsIsWithheld(t *testing.T) {
	keepTheDefaultLogger(t)
	var out bytes.Buffer
	SetDefault(New(&out, "info"))

	answer := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{
		{Content: &genai.Content{Parts: []*genai.Part{{Text: "first answer"}}}},
		{Content: &genai.Content{Parts: []*genai.Part{{Text: "second answer"}}}},
	}}
	_ = answer.Text()
	log.Printf("an app line %d", 7)

	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		records = append(records, decodeJSONLine(t, line))
	}
	if len(records) != 2 {
		t.Fatalf("want genai's record and the app's, got %d: %s", len(records), out.String())
	}
	if records[0]["message"] != SDKLineWithheld || records[0]["sdk"] != "google.golang.org/genai" {
		t.Errorf("genai's line was written as %v", records[0])
	}
	if records[1]["message"] != "an app line 7" {
		t.Errorf("the app's own line was written as %v", records[1])
	}
}

// providerError is an error with the fields a provider's error names for a
// log record, as providererr.Error is.
type providerError struct{}

func (providerError) Error() string { return "openai answered HTTP 400 (bad_content)" }
func (providerError) LogAttrs() []slog.Attr {
	return []slog.Attr{slog.String("provider", "openai"), slog.Int("status", 400), slog.String("code", "bad_content")}
}

// An error attribute that holds a provider's error is written as its text and
// its fields, bound or not, wrapped or not; any other error as its text.
func TestAProviderErrorIsWrittenWithItsFields(t *testing.T) {
	var out bytes.Buffer
	logger := New(&out, "info")
	logger.Warn("summarization failed", "error", fmt.Errorf("openai completion: %w", providerError{}))
	logger.With("error", providerError{}).Warn("bound")
	logger.Warn("plain", "error", fmt.Errorf("disk full"))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want three records: %s", out.String())
	}
	for i, wantText := range []string{"openai completion: openai answered HTTP 400 (bad_content)", "openai answered HTTP 400 (bad_content)"} {
		var record struct {
			Error struct {
				Text, Provider, Code string
				Status               int
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
			t.Fatalf("record %d: %v: %s", i, err, lines[i])
		}
		if e := record.Error; e.Text != wantText || e.Provider != "openai" || e.Status != 400 || e.Code != "bad_content" {
			t.Errorf("record %d's error = %+v, want its text and fields: %s", i, e, lines[i])
		}
	}
	if plain := decodeJSONLine(t, lines[2]); plain["error"] != "disk full" {
		t.Errorf("another error was written as %v", plain["error"])
	}
}
