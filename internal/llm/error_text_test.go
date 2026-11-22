package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// plantedWord stands for a meeting's words in the requests these tests send.
const plantedWord = "quokkaplantedroomword"

// TestAProviderErrorDoesNotQuoteTheRequest: a provider can quote in its error
// message the request it was sent, and a summary's request is a transcript.
// The error is logged, kept as the session's summary error and shown, so it
// keeps the provider's status and error code and never the provider's
// message. Each provider answers status 400 with its own error shape, whose
// message quotes the whole request body.
func TestAProviderErrorDoesNotQuoteTheRequest(t *testing.T) {
	for _, tc := range []struct {
		provider, code string
		body           func(request string) string
		client         func(t *testing.T, url string) Client
	}{
		{
			provider: "openai", code: "bad_content",
			body: func(request string) string {
				return fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error","code":"bad_content"}}`, "Invalid content: "+request)
			},
			client: func(t *testing.T, url string) Client {
				c, err := NewClient(keyProvider(t, config.APIOpenAI, url+"/v1"), "gpt-test")
				if err != nil {
					t.Fatal(err)
				}
				return c
			},
		},
		{
			provider: "anthropic", code: "invalid_request_error",
			body: func(request string) string {
				return fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, "Invalid content: "+request)
			},
			client: func(t *testing.T, url string) Client {
				c, err := NewClient(keyProvider(t, config.APIAnthropic, url), "claude-test")
				if err != nil {
					t.Fatal(err)
				}
				return c
			},
		},
		{
			provider: "gemini", code: "INVALID_ARGUMENT",
			body: func(request string) string {
				return fmt.Sprintf(`{"error":{"code":400,"message":%q,"status":"INVALID_ARGUMENT"}}`, "Invalid content: "+request)
			},
			client: func(t *testing.T, url string) Client {
				c, err := NewClient(keyProvider(t, config.APIGemini, url), "gemini-test")
				if err != nil {
					t.Fatal(err)
				}
				return c
			},
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			quoted := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, _ := io.ReadAll(r.Body)
				if strings.Contains(string(request), plantedWord) {
					quoted++
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body(string(request)))
			}))
			t.Cleanup(server.Close)
			client := tc.client(t, server.URL)
			messages := []Message{{Role: "system", Content: "Summarize."}, {Role: "user", Content: "We agreed on " + plantedWord + " for the launch."}}

			_, completeErr := client.Complete(context.Background(), messages)
			_, jsonErr := client.CompleteJSON(context.Background(), messages, JSONSchema(map[string]string{"summary": "string"}))
			if quoted != 2 {
				t.Fatalf("the provider quoted the planted word in %d of 2 answers, so the check read nothing", quoted)
			}
			for name, err := range map[string]error{"Complete": completeErr, "CompleteJSON": jsonErr} {
				if err == nil {
					t.Fatalf("%s: no error from an answer of status 400", name)
				}
				if strings.Contains(err.Error(), plantedWord) || strings.Contains(err.Error(), "Invalid content") {
					t.Errorf("%s: the error quotes the provider's message: %v", name, err)
				}
				if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), tc.code) {
					t.Errorf("%s: the error %q does not name status 400 and code %s", name, err, tc.code)
				}
			}
		})
	}
}
