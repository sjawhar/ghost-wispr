package tts

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
)

// panickingProvider panics in Synthesize with a value that quotes the
// planted word, as a provider's library could with what it was answered.
type panickingProvider struct{}

func (panickingProvider) Name() string { return "gemini" }

func (panickingProvider) Synthesize(context.Context, SpeechRequest) (*SpeechResponse, error) {
	panic("response quoted " + plantedWord)
}

// A panic in synthesis fails the request with a fixed description, and its
// log record names the panic's type, the provider and the stack, never the
// panic's value, which can quote what the provider answered.
func TestAPanicInSynthesisRecordsNoValue(t *testing.T) {
	logged := logtest.Capture(t)
	o := NewOrchestrator(panickingProvider{}, nil, OrchestratorOpts{})
	t.Cleanup(o.Stop)
	id, err := o.Speak("hello", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, err := o.WaitForSynthesis(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusFailed {
		t.Fatalf("status = %q, want a failed request, so the panic was not recovered", status.Status)
	}
	if strings.Contains(status.Error, plantedWord) || status.Error != "internal error: synthesis panicked" {
		t.Errorf("status error = %q, want the fixed description", status.Error)
	}
	var recovered *logtest.Record
	for _, record := range logged.Records() {
		text, _ := json.Marshal(record)
		if strings.Contains(string(text), plantedWord) {
			t.Errorf("a record holds the panic's value: %s", text)
		}
		if record.Message == "tts orchestrator panic recovered" {
			recovered = &record
		}
	}
	if recovered == nil {
		t.Fatalf("no record of the recovered panic: %+v", logged.Records())
	}
	stack, _ := recovered.Attrs["stack"].(string)
	if recovered.Attrs["panic_type"] != "string" || recovered.Attrs["provider"] != "gemini" || !strings.Contains(stack, "panickingProvider.Synthesize") {
		t.Errorf("the record does not name the panic's type, the provider and a stack through Synthesize: %+v", recovered.Attrs)
	}
}

// The model a Gemini answer reports is kept, for the log and the request's
// status, only when it is one plain token, and as "unknown" otherwise: the
// answer's text can be anything.
func TestGeminiKeepsOnlyAPlainModelName(t *testing.T) {
	for modelVersion, want := range map[string]string{
		"gemini-3.8-flash-tts":                "gemini-3.8-flash-tts",
		"the room said " + plantedWord:        "unknown",
		"":                                    "not reported by provider",
		plantedWord + "\nsecond line":         "unknown",
		strings.Repeat("x", 80) + plantedWord: "unknown",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"modelVersion": modelVersion,
				"candidates":   []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"data": base64.StdEncoding.EncodeToString(silentWAV())}}}}}},
			})
		}))
		p, err := NewGeminiProvider("project", "model", "Kore", WithGeminiAuthCredentials(auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: staticToken{}})), WithGeminiBaseURL(server.URL))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.Synthesize(context.Background(), SpeechRequest{Text: "hello"})
		server.Close()
		if err != nil {
			t.Fatalf("modelVersion %q: Synthesize: %v", modelVersion, err)
		}
		if resp.Model != want {
			t.Errorf("modelVersion %q: model = %q, want %q", modelVersion, resp.Model, want)
		}
	}
}

// silentWAV is a 24 kHz mono PCM16 WAV of one silent sample.
func silentWAV() []byte {
	wav := make([]byte, 46)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], 38)
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], 24000)
	binary.LittleEndian.PutUint32(wav[28:32], 48000)
	binary.LittleEndian.PutUint16(wav[32:34], 2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], 2)
	return wav
}
