package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// observedWebhookContext signals when the handler reaches its blocking
// delivery select, without sleeping or inspecting receiver internals.
type observedWebhookContext struct {
	context.Context
	once  sync.Once
	ready chan struct{}
}

func (c *observedWebhookContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestAWebhookRetryIsNotAcknowledgedBeforeDelivery(t *testing.T) {
	events := make(chan webhookEvent)
	r, err := newWebhookReceiver(webhookTestSecret, events, newLogger(io.Discard, "bridge", slog.LevelDebug))
	if err != nil {
		t.Fatal(err)
	}
	body, signature := webhookSignedLog(t, "overlapping-attempts")
	firstCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	post := func(ctx context.Context) (<-chan int, <-chan struct{}) {
		observed := &observedWebhookContext{Context: ctx, ready: make(chan struct{})}
		result := make(chan int, 1)
		go func() {
			req := httptest.NewRequestWithContext(observed, http.MethodPost, webhookPath, bytes.NewReader(body))
			req.Header.Set("X-Webhook-Signature", signature)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			result <- w.Code
		}()
		return result, observed.ready
	}
	first, ready := post(firstCtx)
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("the first attempt did not reach delivery")
	}
	second, waiting := post(t.Context())
	select {
	case code := <-second:
		t.Fatalf("retry returned %d before either request delivered its event", code)
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("the retry neither waited nor returned")
	}
	cancel()
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("the retry did not deliver after the original request was canceled")
	}
	if code := <-first; code != http.StatusServiceUnavailable {
		t.Errorf("canceled attempt returned %d, want 503", code)
	}
	if code := <-second; code != http.StatusOK {
		t.Errorf("delivered retry returned %d, want 200", code)
	}
}
