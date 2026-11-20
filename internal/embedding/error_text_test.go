package embedding

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plantedWord stands for a meeting's words in the texts these tests embed.
const plantedWord = "quokkaplantedroomword"

// TestAnEmbeddingErrorDoesNotQuoteTheText: the texts embedded are a
// session's transcript, and a provider can quote them in its error message.
// The error is logged, so it keeps the provider's status and error code and
// never the provider's message.
func TestAnEmbeddingErrorDoesNotQuoteTheText(t *testing.T) {
	for _, tc := range []struct {
		provider, code string
		body           func(request string) string
		client         func(url string) (Client, error)
	}{
		{
			provider: "openai", code: "bad_content",
			body: func(request string) string {
				return fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error","code":"bad_content"}}`, "Invalid input: "+request)
			},
			client: func(url string) (Client, error) {
				return newOpenAIClient("test-key", "embed-test", &clientOptions{baseURL: url + "/v1"})
			},
		},
		{
			provider: "gemini", code: "INVALID_ARGUMENT",
			body: func(request string) string {
				return fmt.Sprintf(`{"error":{"code":400,"message":%q,"status":"INVALID_ARGUMENT"}}`, "Invalid input: "+request)
			},
			client: func(url string) (Client, error) {
				return newGeminiClient("test-key", "embed-test", &clientOptions{baseURL: url})
			},
		},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			quoted := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, _ := io.ReadAll(r.Body)
				quoted = quoted || strings.Contains(string(request), plantedWord)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.body(string(request)))
			}))
			t.Cleanup(server.Close)
			client, err := tc.client(server.URL)
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Embed(context.Background(), []string{"We agreed on " + plantedWord + "."}, TaskTypeDocument)
			if !quoted {
				t.Fatal("the provider was not sent the planted word, so the check read nothing")
			}
			if err == nil {
				t.Fatal("no error from an answer of status 400")
			}
			if strings.Contains(err.Error(), plantedWord) || strings.Contains(err.Error(), "Invalid input") {
				t.Errorf("the error quotes the provider's message: %v", err)
			}
			if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), tc.code) {
				t.Errorf("the error %q does not name status 400 and code %s", err, tc.code)
			}
		})
	}
}
