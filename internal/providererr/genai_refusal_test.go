package providererr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestARefusedGenAIBatchKeepsItsStatusAndCode fails if a refusal genai
// returns for a batch create, as its own client builds it from the answer,
// loses its HTTP status and code to "withheld", or keeps the message, which
// can quote the request.
func TestARefusedGenAIBatchKeepsItsStatusAndCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied by policy for `+planted+`"}}`)
	}))
	defer srv.Close()
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI, APIKey: "k-1", HTTPClient: srv.Client(),
		HTTPOptions: genai.HTTPOptions{BaseURL: srv.URL + "/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Batches.Create(context.Background(), "gemini-example", &genai.BatchJobSource{
		InlinedRequests: []*genai.InlinedRequest{{Contents: genai.Text("summarize " + planted)}},
	}, nil)
	if err == nil {
		t.Fatal("a refused batch create succeeded")
	}
	got := From("vertex", err).Error()
	if got != "vertex answered HTTP 403 (PERMISSION_DENIED)" || strings.Contains(got, planted) {
		t.Fatalf("From = %q; want the status and code alone", got)
	}
}
