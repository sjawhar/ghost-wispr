package main

import (
	"context"
	"errors"
	"fmt"
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
// once and again each refresh, or a failed read drops the keys known. The
// test lets each read answer in turn, since the first read runs in the
// background (TestProjectKeysDoNotHoldTheStart).
func TestProjectKeysAreReadDaily(t *testing.T) {
	answers := [][]string{{"INFRA"}, nil, {"INFRA", "OSSV"}}
	step := make(chan []string)
	read := func(ctx context.Context) ([]string, error) {
		select {
		case got := <-step:
			if got == nil {
				return nil, errors.New("dispatch: no answer")
			}
			return got, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var keys projectKeys
	watchProjectKeys(ctx, read, 10*time.Millisecond, &keys, t.Logf)
	waitKeys := func(want []string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !slices.Equal(keys.Get(), want) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := keys.Get(); !slices.Equal(got, want) {
			t.Fatalf("keys %q, want %q", got, want)
		}
	}
	step <- answers[0] // the first read, at once
	waitKeys(answers[0])
	step <- answers[1] // a refresh that fails keeps the keys known
	step <- answers[2] // the next refresh
	waitKeys(answers[2])
}

// TestProjectKeysDoNotHoldTheStart fails if a Dispatch that does not answer
// holds the unit's start while the project keys are read (ReviewPR89's round
// 2: up to 5 s).
func TestProjectKeysDoNotHoldTheStart(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	read := func(ctx context.Context) ([]string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("dispatch: no answer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var keys projectKeys
	start := time.Now()
	watchProjectKeys(ctx, read, time.Hour, &keys, t.Logf)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("reading the project keys held the start %v", took)
	}
}

// TestProjectKeysLogNothingOnceStopped fails if the watcher logs a read that
// failed because its context ended, as at shutdown: the log is no warning
// then, and in tests it ran past the test's end (the data race seen once at
// 8e6a143 and again in a 10-run pass, project_keys.go:44).
func TestProjectKeysLogNothingOnceStopped(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	read := func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, errors.New("dispatch: no answer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var keys projectKeys
	watchProjectKeys(ctx, read, time.Hour, &keys, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	})
	cancel()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 0 {
		t.Fatalf("the stopped watcher logged %q", logs)
	}
}
