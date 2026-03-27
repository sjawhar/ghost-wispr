package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/llm"
)

type fakeStreamer struct {
	errs       []error
	reopenErrs []error
	calls      int
	reopens    int
}

func (f *fakeStreamer) Stream(_ io.Writer) error {
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeStreamer) Reopen() error {
	f.reopens++
	if len(f.reopenErrs) == 0 {
		return nil
	}
	err := f.reopenErrs[0]
	f.reopenErrs = f.reopenErrs[1:]
	return err
}

func TestStreamMicWithRetryRetriesOverflow(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("Input overflowed"), nil}}
	var waits int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(_ time.Duration) {
		waits++
	}, func(string, ...any) {}, nil)

	if streamer.calls != 2 {
		t.Fatalf("expected 2 stream calls, got %d", streamer.calls)
	}
	if waits != 1 {
		t.Fatalf("expected 1 wait call, got %d", waits)
	}
}

func TestStreamMicWithRetryOverflowDoesNotReopen(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("Input overflowed"), nil}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(_ time.Duration) {}, func(string, ...any) {}, nil)

	if streamer.reopens != 0 {
		t.Fatalf("expected 0 reopen calls for overflow, got %d", streamer.reopens)
	}
}

func TestStreamMicWithRetryRetriesDeviceError(t *testing.T) {
	streamer := &fakeStreamer{errs: []error{errors.New("device disconnected"), nil}}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {}, nil)

	if streamer.calls != 2 {
		t.Fatalf("expected 2 stream calls, got %d", streamer.calls)
	}
	if streamer.reopens != 1 {
		t.Fatalf("expected 1 reopen call, got %d", streamer.reopens)
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("expected 1 wait of 1s, got %v", waits)
	}
}

func TestStreamMicWithRetryBacksOffOnReopenFailure(t *testing.T) {
	streamer := &fakeStreamer{
		errs:       []error{errors.New("ENODEV"), errors.New("ENODEV"), errors.New("ENODEV")},
		reopenErrs: []error{errors.New("no device"), errors.New("no device"), errors.New("no device")},
	}
	var waits []time.Duration
	waitCount := make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for i := 0; i < 3; i++ {
			<-waitCount
		}
		cancel()
	}()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
		waitCount <- struct{}{}
	}, func(string, ...any) {}, nil)

	if waits[0] != time.Second {
		t.Fatalf("expected first wait 1s, got %v", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Fatalf("expected second wait 2s, got %v", waits[1])
	}
	if waits[2] != 4*time.Second {
		t.Fatalf("expected third wait 4s, got %v", waits[2])
	}
}

func TestStreamMicWithRetryResetsBackoffOnSuccessfulReopen(t *testing.T) {
	streamer := &fakeStreamer{
		errs:       []error{errors.New("err1"), errors.New("err2"), errors.New("err3"), nil},
		reopenErrs: []error{errors.New("no device"), nil, nil},
	}
	var waits []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamMicWithRetry(ctx, streamer, bytes.NewBuffer(nil), func(d time.Duration) {
		waits = append(waits, d)
	}, func(string, ...any) {}, nil)

	// err1 → 1s wait, reopen fails → err2 (still in loop) → 2s wait, reopen succeeds (reset) → err3 → 1s wait, reopen succeeds → nil
	if len(waits) != 3 {
		t.Fatalf("expected 3 waits, got %d: %v", len(waits), waits)
	}
	if waits[0] != time.Second {
		t.Fatalf("expected first wait 1s, got %v", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Fatalf("expected second wait 2s, got %v", waits[1])
	}
	if waits[2] != time.Second {
		t.Fatalf("expected third wait 1s (reset), got %v", waits[2])
	}
}

type mockLLMClient struct {
	completeErr error
}

func (m *mockLLMClient) Complete(_ context.Context, _ []llm.Message) (string, error) {
	return "", m.completeErr
}

func (m *mockLLMClient) CompleteJSON(_ context.Context, _ []llm.Message, _ map[string]any) (json.RawMessage, error) {
	return nil, m.completeErr
}

func TestValidateLLMClient_ReturnsErrorForBadKey(t *testing.T) {
	t.Run("factory error", func(t *testing.T) {
		factory := func(provider, model string) (llm.Client, error) {
			return nil, errors.New("invalid API key")
		}
		err := validateLLMClient(context.Background(), factory, "openai", "gpt-4")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid API key") {
			t.Fatalf("expected error to contain 'invalid API key', got: %v", err)
		}
	})

	t.Run("complete error", func(t *testing.T) {
		factory := func(provider, model string) (llm.Client, error) {
			return &mockLLMClient{completeErr: errors.New("401 Unauthorized")}, nil
		}
		err := validateLLMClient(context.Background(), factory, "openai", "gpt-4")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "401 Unauthorized") {
			t.Fatalf("expected error to contain '401 Unauthorized', got: %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		factory := func(provider, model string) (llm.Client, error) {
			return &mockLLMClient{}, nil
		}
		err := validateLLMClient(context.Background(), factory, "openai", "gpt-4")
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})
}
