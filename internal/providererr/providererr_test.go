package providererr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/auth"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

const planted = "quokkaplantedroomword"

// Each provider error From knows is reduced to its provider, status and code:
// none of the provider's message stays, the code stays only when it is a plain
// token, and errors.As still finds the provider's own error.
func TestFromKeepsOnlyStatusAndCode(t *testing.T) {
	message := "Invalid content: We agreed on " + planted + "."
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"openai API error", &openai.APIError{HTTPStatusCode: 400, Code: "bad_content", Type: "invalid_request_error", Message: message}, "openai answered HTTP 400 (bad_content)"},
		{"openai API error with a numeric code", &openai.APIError{HTTPStatusCode: 400, Code: 7, Type: "invalid_request_error", Message: message}, "openai answered HTTP 400 (invalid_request_error)"},
		{"openai request error", &openai.RequestError{HTTPStatusCode: 502, Body: []byte(message), Err: errors.New(message)}, "openai answered HTTP 502"},
		{"gemini API error", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: message}, "gemini answered HTTP 400 (INVALID_ARGUMENT)"},
		{"gemini API error pointer", &genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: message}, "gemini answered HTTP 429 (RESOURCE_EXHAUSTED)"},
		{"drive error", &googleapi.Error{Code: 403, Message: message, Body: message, Errors: []googleapi.ErrorItem{{Reason: "insufficientFilePermissions", Message: message}}}, "drive answered HTTP 403 (insufficientFilePermissions)"},
		{"drive error with a sentence for a reason", &googleapi.Error{Code: 400, Message: message, Errors: []googleapi.ErrorItem{{Reason: "We agreed " + planted}}}, "drive answered HTTP 400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := strings.Fields(tc.want)[0]
			wrapped := fmt.Errorf("call: %w", tc.err)
			got := From(provider, wrapped)
			if got.Error() != tc.want {
				t.Errorf("From = %q, want %q", got, tc.want)
			}
			if strings.Contains(got.Error(), planted) || strings.Contains(fmt.Sprintf("ok: %v", got), planted) {
				t.Errorf("From kept the provider's message: %v", got)
			}
			if !errors.Is(got, tc.err) && !errors.As(got, new(genai.APIError)) {
				t.Errorf("errors.Is no longer finds the provider's error in %v", got)
			}
			if inner := errors.Unwrap(got); inner != nil {
				t.Errorf("errors.Unwrap reaches the provider's error, whose text holds its message: %v", inner)
			}
			if From(provider, got) != got {
				t.Error("From reduced a reduced error again")
			}
		})
	}
}

// A transport error loses its URL and keeps a cancel, a deadline, a network
// failure or a failure that says its text is its own (ProviderFailure, as a
// refused redirect's), which hold nothing the provider sent; any other
// transport failure is withheld, since net/http quotes an answer it could
// not read.
func TestFromKeepsOnlyATransportErrorsOwnFailure(t *testing.T) {
	address := "https://api.example/v1?q=" + planted
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	stopped := ownWords("provider \"p\" at https://api.example redirected the call to another origin")
	for _, tc := range []struct {
		err   error
		want  string
		cause error
	}{
		{&url.Error{Op: "Post", URL: address, Err: context.DeadlineExceeded}, "deepgram call failed: Post: context deadline exceeded", context.DeadlineExceeded},
		{&url.Error{Op: "Post", URL: address, Err: fmt.Errorf("read: %w", context.Canceled)}, "deepgram call failed: Post: context canceled", context.Canceled},
		{&url.Error{Op: "Post", URL: address, Err: refused}, "deepgram call failed: Post: dial tcp: connect: connection refused", refused},
		{&url.Error{Op: "Get", URL: address, Err: stopped}, `deepgram call failed: Get: provider "p" at https://api.example redirected the call to another origin`, stopped},
		{&url.Error{Op: "Post", URL: address, Err: fmt.Errorf(`net/http: HTTP/1.x transport connection broken: malformed HTTP status code %q`, planted)}, "deepgram call failed (its error is withheld: it can quote the request or the answer)", nil},
	} {
		got := From("deepgram", tc.err)
		if got.Error() != tc.want || strings.Contains(got.Error(), planted) {
			t.Errorf("From(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if tc.cause != nil && !errors.Is(got, tc.cause) {
			t.Errorf("From(%v) = %v: errors.Is no longer finds %v", tc.err, got, tc.cause)
		}
	}
}

// ownWords is a failure whose text holds nothing a provider sent.
type ownWords string

func (w ownWords) Error() string           { return string(w) }
func (w ownWords) ProviderFailure() string { return string(w) }

// Another error of a call, such as an SDK's failure to read an answer it
// quotes, is withheld; errors.Is still finds it. A wrapped cancel keeps only
// that it was canceled; a bare one is returned as it is.
func TestFromWithholdsOtherErrors(t *testing.T) {
	cause := fmt.Errorf("failed to unmarshal response: {\"text\":%q}: unexpected end of JSON", planted)
	got := From("gemini", cause)
	if strings.Contains(got.Error(), planted) || !errors.Is(got, cause) {
		t.Fatalf("From = %v: keeps the error's text, or loses the error", got)
	}
	if got.Error() != "gemini call failed (its error is withheld: it can quote the request or the answer)" {
		t.Errorf("From = %q", got)
	}
	canceled := fmt.Errorf("stream %s: %w", planted, context.Canceled)
	if got := From("gemini", canceled); got.Error() != "gemini call failed: context canceled" || !errors.Is(got, context.Canceled) {
		t.Errorf("From(%v) = %v, want only that it was canceled", canceled, got)
	}
	if From("gemini", context.Canceled) != context.Canceled {
		t.Error("From changed a bare cancel")
	}
	if From("gemini", nil) != nil {
		t.Fatal("From(nil) is not nil")
	}
}

// A provider saying the request is too long is kept as TooLong, without its
// words, so the summarizer can summarize in parts.
func TestFromKeepsThatTheRequestWasTooLong(t *testing.T) {
	for _, err := range []error{
		&openai.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded", Message: "This model's maximum context length is 8192 tokens"},
		genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "The input token count (1200000) exceeds the maximum number of tokens allowed (1048576)."},
		genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "Bad request", Details: []map[string]any{{"reason": "context length exceeded"}}},
		&googleapi.Error{Code: 400, Message: "prompt is too long: " + planted},
	} {
		var reduced *Error
		if !errors.As(From("openai", err), &reduced) || !reduced.TooLong {
			t.Errorf("From(%v) does not say the request was too long", err)
		}
		if strings.Contains(reduced.Error(), planted) {
			t.Errorf("From kept the provider's message: %v", reduced)
		}
	}
	var reduced *Error
	if errors.As(From("openai", &openai.APIError{HTTPStatusCode: 400, Code: "bad_content", Message: "bad"}), &reduced) && reduced.TooLong {
		t.Error("a request refused for another reason reads as too long")
	}
}

// A Google auth error keeps its status and the OAuth error code of its body,
// never its text.
func TestFromReducesGoogleAuthErrors(t *testing.T) {
	body := `{"error":"invalid_grant","error_description":"cannot authenticate ` + planted + `"}`
	authErr := &auth.Error{Response: &http.Response{StatusCode: 403}, Body: []byte(body)}
	if got := From("google", fmt.Errorf("get token: %w", authErr)); got.Error() != "google answered HTTP 403 (invalid_grant)" || !errors.Is(got, authErr) {
		t.Errorf("From(auth error) = %q", got)
	}
}

// An error From cannot read is withheld whole, its status unknown: a status
// its text names can be the answer's own words, as in an SDK's failure to
// decode a 200 that quotes "status code 403". Unknown, it is retried.
func TestFromReadsNoStatusFromErrorText(t *testing.T) {
	err := fmt.Errorf("failed to unmarshal response: {\"text\":\"mentions status code 403 and %s\"}", planted)
	got := From("gemini", err)
	var reduced *Error
	if !errors.As(got, &reduced) || reduced.Status != 0 || !reduced.Retryable() || strings.Contains(got.Error(), "403") {
		t.Fatalf("From = %q (%+v), want it withheld with no status, and retried", got, reduced)
	}
	reduced.Status = 403 // a caller that knows its SDK's format sets it
	if got.Error() != "gemini call failed with HTTP 403 (its error is withheld: it can quote the request or the answer)" || reduced.Retryable() {
		t.Errorf("a withheld error with a known 403 = %q, retryable %v", got, reduced.Retryable())
	}
}

// An OpenAI answer go-openai could only partly read comes as a RequestError
// holding an APIError with no status; the reduced error keeps the answer's
// status, which retries read, and the code.
func TestFromKeepsAPartlyReadOpenAIAnswersStatus(t *testing.T) {
	partial := &openai.RequestError{HTTPStatusCode: 400, Body: []byte(planted), Err: &openai.APIError{Code: "bad_content", Message: planted}}
	if got := From("openai", partial).Error(); got != "openai answered HTTP 400 (bad_content)" {
		t.Fatalf("From = %q, want the status and the code", got)
	}
}

// The fields a log record writes of a reduced error are its provider,
// status, code and flags, each only when set.
func TestLogAttrs(t *testing.T) {
	got := map[string]any{}
	for _, attr := range New("gemini", 429, "RESOURCE_EXHAUSTED", "context length exceeded "+planted).LogAttrs() {
		got[attr.Key] = attr.Value.Any()
	}
	want := map[string]any{"provider": "gemini", "status": int64(429), "code": "RESOURCE_EXHAUSTED", "too_long": true}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("LogAttrs = %v, want %v", got, want)
	}
	withheld := map[string]any{}
	for _, attr := range From("gemini", errors.New(planted)).(*Error).LogAttrs() {
		withheld[attr.Key] = attr.Value.Any()
	}
	if fmt.Sprint(withheld) != fmt.Sprint(map[string]any{"provider": "gemini", "withheld": true}) {
		t.Errorf("a withheld error's LogAttrs = %v", withheld)
	}
}

// Reduce reads a code from the named fields of a JSON body, only as a plain
// token, and never keeps the body.
func TestReduce(t *testing.T) {
	body := []byte(`{"err_code":"INVALID_QUERY_PARAMETER","err_msg":"Could not use ` + planted + `"}`)
	if got := Reduce("deepgram", http.StatusBadRequest, body, "err_code").Error(); got != "deepgram answered HTTP 400 (INVALID_QUERY_PARAMETER)" {
		t.Errorf("Reduce = %q", got)
	}
	if got := Reduce("deepgram", http.StatusBadRequest, []byte(`{"err_code":"Bad `+planted+`"}`), "err_code").Error(); got != "deepgram answered HTTP 400" {
		t.Errorf("Reduce kept a code that is no token: %q", got)
	}
	if got := Reduce("deepgram", http.StatusBadGateway, []byte("<html>"+planted), "err_code").Error(); got != "deepgram answered HTTP 502" {
		t.Errorf("Reduce = %q", got)
	}
}
