package tts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
)

// plantedWord stands for one room word in the text to speak: a single
// lower-case word, which a filter that looks for a quote of the text cannot
// tell from an ordinary word.
const plantedWord = "quokkaplantedroomword"

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
	id, err := o.Speak("hello", "")
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
