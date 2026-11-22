package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/logtest"
)

// TestLiveOptionsCarryTheProjectKeys fails if Dispatch's project keys are
// not keyterms of the live Deepgram request, after the wake phrases, so that
// "infra" is not heard as "in for" (R4; end-to-end run 8c).
func TestLiveOptionsCarryTheProjectKeys(t *testing.T) {
	cfg := deepgramKeyConfig("")
	cfg.SpeakerEnabled, cfg.Voice.WakePhrases = true, []string{"Wispr", "Ghost Wispr"}
	if got, want := liveTranscriptionOptions(&cfg, 16000, []string{"INFRA", "ABC"}).Keyterm, []string{"Wispr", "Whisper", "Ghost Wispr", "INFRA", "ABC"}; !slices.Equal(got, want) {
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
	watchProjectKeys(ctx, read, 10*time.Millisecond, &keys)
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
	watchProjectKeys(ctx, read, time.Hour, &keys)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("reading the project keys held the start %v", took)
	}
}

// TestAFailedProjectKeysReadWarns fails if a read of the project keys that
// fails while the watcher runs is not a WARN record, which a unit with
// log_level: warn keeps. It is also the control of
// TestProjectKeysLogNothingOnceStopped: the record that test looks for.
func TestAFailedProjectKeysReadWarns(t *testing.T) {
	logged := logtest.Capture(t)
	read := func(context.Context) ([]string, error) { return nil, errors.New("dispatch: no answer") }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var keys projectKeys
	watchProjectKeys(ctx, read, time.Hour, &keys)
	deadline := time.Now().Add(2 * time.Second)
	for len(projectKeysRecords(logged)) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	records := projectKeysRecords(logged)
	if len(records) != 1 || records[0].Level != slog.LevelWarn.String() || records[0].Attrs["error"] != "dispatch: no answer" {
		t.Fatalf("records of the failed read = %+v, want one WARN record with its error", records)
	}
}

// TestProjectKeysLogNothingOnceStopped fails if the watcher logs a read that
// failed because its context ended, as at shutdown: the log is no warning
// then, and in tests it ran past the test's end (the data race seen once at
// 8e6a143 and again in a 10-run pass, project_keys.go:44).
func TestProjectKeysLogNothingOnceStopped(t *testing.T) {
	logged := logtest.Capture(t)
	read := func(ctx context.Context) ([]string, error) {
		<-ctx.Done()
		return nil, errors.New("dispatch: no answer")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var keys projectKeys
	watchProjectKeys(ctx, read, time.Hour, &keys)
	cancel()
	time.Sleep(200 * time.Millisecond)
	if records := projectKeysRecords(logged); len(records) != 0 {
		t.Fatalf("the stopped watcher logged %+v", records)
	}
}

// projectKeysRecords returns the records the project keys watcher wrote.
func projectKeysRecords(logged *logtest.Log) []logtest.Record {
	var records []logtest.Record
	for _, record := range logged.Records() {
		if strings.Contains(record.Message, "project keys") {
			records = append(records, record)
		}
	}
	return records
}
