package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// TestLiveOptionsCarryTheProjectKeys fails if Dispatch's project keys are
// not keyterms of the live Deepgram request, after the wake phrases, so that
// "infra" is not heard as "in for" (R4; end-to-end run 8c).
func TestLiveOptionsCarryTheProjectKeys(t *testing.T) {
	cfg := config.Config{DeepgramModel: "nova-3", SpeakerEnabled: true, Voice: config.Voice{WakePhrases: []string{"Wispr", "Ghost Wispr"}}}
	if got, want := liveTranscriptionOptions(&cfg, 16000, []string{"INFRA", "ABC"}).Keyterm, []string{"Wispr", "Ghost Wispr", "INFRA", "ABC"}; !slices.Equal(got, want) {
		t.Fatalf("keyterms %q, want %q", got, want)
	}
}

// TestProjectKeysAreReadDaily fails if the project keys are not read at
// once and again each refresh, or a failed read drops the keys known.
func TestProjectKeysAreReadDaily(t *testing.T) {
	var mu sync.Mutex
	answers := [][]string{{"INFRA"}, nil, {"INFRA", "OSSV"}}
	calls := 0
	read := func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		i := min(calls, len(answers)-1)
		calls++
		if answers[i] == nil {
			return nil, errors.New("dispatch: no answer")
		}
		return answers[i], nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var keys projectKeys
	watchProjectKeys(ctx, read, 50*time.Millisecond, &keys, t.Logf)
	if got := keys.Get(); !slices.Equal(got, []string{"INFRA"}) {
		t.Fatalf("keys at start %q, want the first read's", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !slices.Equal(keys.Get(), []string{"INFRA", "OSSV"}) && time.Now().Before(deadline) {
		if got := keys.Get(); len(got) == 0 {
			t.Fatal("a failed read dropped the keys known")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := keys.Get(); !slices.Equal(got, []string{"INFRA", "OSSV"}) {
		t.Fatalf("keys after the refreshes %q, want the newest read's", got)
	}
}
