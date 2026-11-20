// Package providererr keeps a provider's own words out of Ghost Wispr's
// errors. A provider (Deepgram, Gemini, OpenAI, Anthropic, Google Drive) can
// quote in its error message the request it was sent, and a request can hold
// a meeting's words: a transcript, a summary, a session's title, a question
// asked of the voice. These errors go to the app's log (the journal and
// GET /api/logs), and some are kept and shown, so an error from a provider
// keeps only which provider answered, the HTTP status and the provider's
// error code.
package providererr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strings"

	"cloud.google.com/go/auth"
	"github.com/anthropics/anthropic-sdk-go"
	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/api/googleapi"
	"google.golang.org/genai"
)

// Error is a provider's answer of an error, or another failure of a call to
// a provider, without any text the provider sent.
type Error struct {
	// Provider is who answered: "openai", "anthropic", "gemini", "drive",
	// "deepgram".
	Provider string
	// Status is the HTTP status, 0 when there is none.
	Status int
	// Code is the provider's error code, such as "invalid_request_error" or
	// "INVALID_ARGUMENT", kept only when it is a plain token (Token).
	Code string
	// TooLong is whether the provider said the request is longer than the
	// model takes: the summarizer then summarizes the transcript in parts.
	TooLong bool
	// Failure is why a call got no answer, when the provider sent no text
	// of it: a cancel, a deadline, or a network failure ("Post: dial tcp
	// ...: connection refused").
	Failure string
	// Withheld is a failure of the call whose text can quote what the
	// provider sent, such as an SDK's failure to read the answer: only that
	// the call failed is kept.
	Withheld bool

	err error // the error From reduced, for errors.As and errors.Is; never printed
}

func (e *Error) Error() string {
	switch {
	case e.Withheld && e.Status != 0:
		return fmt.Sprintf("%s call failed with HTTP %d (its error is withheld: it can quote the request or the answer)", e.Provider, e.Status)
	case e.Withheld:
		return e.Provider + " call failed (its error is withheld: it can quote the request or the answer)"
	case e.Failure != "":
		return e.Provider + " call failed: " + e.Failure
	}
	text := e.Provider + " answered"
	if e.Status != 0 {
		text += fmt.Sprintf(" HTTP %d", e.Status)
	}
	if e.Code != "" {
		text += " (" + e.Code + ")"
	}
	if e.TooLong {
		text += ": the request is too long for the model"
	}
	return text
}

// As lets errors.As find the error From reduced (a Drive 404, say). Error has
// no Unwrap: code that walks a chain with errors.Unwrap and prints its
// innermost error stops here, at text that holds nothing the provider sent.
func (e *Error) As(target any) bool {
	return e.err != nil && errors.As(e.err, target)
}

// Is lets errors.Is find the error From reduced.
func (e *Error) Is(target error) bool {
	return e.err != nil && errors.Is(e.err, target)
}

// LogAttrs are the error's fields for a log record, each only when set:
// provider, status, code, too_long, failure, withheld. The app's logger writes
// an error attribute that holds an *Error as these fields beside the error's
// text (logging.New).
func (e *Error) LogAttrs() []slog.Attr {
	attrs := []slog.Attr{slog.String("provider", e.Provider)}
	if e.Status != 0 {
		attrs = append(attrs, slog.Int("status", e.Status))
	}
	if e.Code != "" {
		attrs = append(attrs, slog.String("code", e.Code))
	}
	if e.TooLong {
		attrs = append(attrs, slog.Bool("too_long", true))
	}
	if e.Failure != "" {
		attrs = append(attrs, slog.String("failure", e.Failure))
	}
	if e.Withheld {
		attrs = append(attrs, slog.Bool("withheld", true))
	}
	return attrs
}

// New returns the error of a provider's answer of status with code (left out
// unless it is a plain token) and the provider's message, which is read only
// for whether the request was too long (TooLong) and never kept.
func New(provider string, status int, code, message string) *Error {
	code = Token(code)
	return &Error{Provider: provider, Status: status, Code: code, TooLong: tooLong(code + " " + message)}
}

// From returns err, the error of a call to a provider's client, reduced to an
// *Error: an OpenAI, Anthropic, Gemini (genai) or Google API (Drive) answer
// keeps its status and code; a cancel, a deadline or a network failure keeps
// its own text, without a URL, which can name what was asked; any other
// failure, such as an SDK's failure to read an answer it quotes or an answer
// net/http could not read, is withheld. A bare context.Canceled or
// DeadlineExceeded is returned as it is. From is called on the client's own
// error, before Ghost Wispr wraps it.
func From(provider string, err error) error {
	if err == nil || err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	var (
		openaiAPI     *openai.APIError
		openaiRequest *openai.RequestError
		gemini        genai.APIError
		geminiPointer *genai.APIError
		anthropicAPI  *anthropic.Error
		google        *googleapi.Error
		googleAuth    *auth.Error
		transport     *url.Error
	)
	var reduced *Error
	switch {
	case errors.As(err, &reduced):
		return err
	case errors.As(err, &openaiRequest):
		// go-openai returns an answer whose error it could only partly read
		// as a RequestError holding that partial APIError, whose status is 0.
		code, message := "", string(openaiRequest.Body)
		if errors.As(openaiRequest.Err, &openaiAPI) {
			code, message = openaiCode(openaiAPI), message+" "+openaiAPI.Message
		}
		reduced = New(provider, openaiRequest.HTTPStatusCode, code, message)
	case errors.As(err, &openaiAPI):
		reduced = New(provider, openaiAPI.HTTPStatusCode, openaiCode(openaiAPI), openaiAPI.Message)
	case errors.As(err, &gemini):
		reduced = New(provider, gemini.Code, gemini.Status, fmt.Sprint(gemini.Message, gemini.Details))
	case errors.As(err, &geminiPointer):
		reduced = New(provider, geminiPointer.Code, geminiPointer.Status, fmt.Sprint(geminiPointer.Message, geminiPointer.Details))
	case errors.As(err, &anthropicAPI):
		var body struct {
			Error struct{ Type, Message string } `json:"error"`
		}
		_ = json.Unmarshal([]byte(anthropicAPI.RawJSON()), &body)
		reduced = New(provider, anthropicAPI.StatusCode, body.Error.Type, body.Error.Message)
	case errors.As(err, &google):
		code := ""
		messages := []string{google.Message, google.Body}
		for _, item := range google.Errors {
			if code == "" {
				code = item.Reason
			}
			messages = append(messages, item.Message)
		}
		reduced = New(provider, google.Code, code, strings.Join(messages, " "))
	case errors.As(err, &googleAuth):
		status := 0
		if googleAuth.Response != nil {
			status = googleAuth.Response.StatusCode
		}
		// An OAuth error names its code in "error"; Google's own JSON errors
		// hold an object there, which is no code.
		reduced = Reduce(provider, status, googleAuth.Body, "error")
	case errors.As(err, &transport):
		reduced = &Error{Provider: provider, Withheld: true}
		if failure := ownFailure(transport.Err); failure != "" {
			reduced = &Error{Provider: provider, Failure: transport.Op + ": " + failure}
		}
	default:
		// Its text can quote the answer, so none of it is kept, nor read: a
		// status it names could be the answer's own words. A caller that knows
		// its SDK's error format can set Status (googleauth does).
		reduced = &Error{Provider: provider, Withheld: true}
		if failure := ownFailure(err); failure != "" {
			reduced = &Error{Provider: provider, Failure: failure}
		}
	}
	reduced.err = err
	return reduced
}

// openaiCode is an OpenAI error's code, or its type when the code is no
// token (OpenAI sends some codes as numbers).
func openaiCode(e *openai.APIError) string {
	if code, _ := e.Code.(string); Token(code) != "" {
		return code
	}
	return e.Type
}

// OwnFailure is a failure whose text holds nothing a provider sent, such as
// a client's refusal of a redirect to another origin: From keeps its
// ProviderFailure text.
type OwnFailure interface {
	error
	ProviderFailure() string
}

// ownFailure is the text of err when it is a failure the provider sent no
// text of: a cancel, a deadline, a network error, or an OwnFailure. It is ""
// otherwise.
func ownFailure(err error) string {
	var network *net.OpError
	var own OwnFailure
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded.Error()
	case errors.As(err, &network):
		return network.Error()
	case errors.As(err, &own):
		return own.ProviderFailure()
	}
	return ""
}

// retryableCodes are provider codes that say a call can succeed if made
// again, whatever its status: Google's quota, availability and deadline
// codes, and OpenAI's, Anthropic's and Drive's rate and server codes.
var retryableCodes = map[string]bool{
	"RESOURCE_EXHAUSTED": true, "UNAVAILABLE": true, "DEADLINE_EXCEEDED": true, "ABORTED": true, "INTERNAL": true,
	"rate_limit_exceeded": true, "rate_limit_error": true, "overloaded_error": true, "api_error": true,
	"server_error": true, "rateLimitExceeded": true, "userRateLimitExceeded": true, "backendError": true,
}

// Retryable is whether the call can succeed if made again: an answer whose
// code says so (retryableCodes), a 408, 429 or 5xx answer, and a failure
// with no answer; not another 4xx answer. The provider's message, which used
// to say "quota" or "try again", is not kept, so the status and code decide.
func (e *Error) Retryable() bool {
	switch {
	case retryableCodes[e.Code], e.Status == 0:
		return true
	case e.Status == 408, e.Status == 429, e.Status >= 500:
		return true
	}
	return e.Status < 400
}

// Reader returns r, the body of a provider's answer, with each read error
// other than io.EOF reduced (From): a failed read can quote the answer, as an
// HTTP/2 GOAWAY's debug text does.
func Reader(provider string, r io.Reader) io.Reader { return answerReader{provider, r} }

type answerReader struct {
	provider string
	r        io.Reader
}

func (a answerReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if err != nil && err != io.EOF {
		err = From(a.provider, err)
	}
	return n, err
}

// rpcCodes are the names of google.rpc.Code, which a Gemini batch job's
// errors carry as numbers.
var rpcCodes = []string{
	"OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND",
	"ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION",
	"ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS",
	"UNAUTHENTICATED",
}

// RPC returns the error of a Google RPC status: provider, the name of code
// (google.rpc.Code; "" when it names none), and its message, which is read
// only for whether the request was too long (New) and never kept.
func RPC(provider string, code int32, message string) *Error {
	name := ""
	if code >= 0 && int(code) < len(rpcCodes) {
		name = rpcCodes[code]
	}
	return New(provider, 0, name, message)
}

// Reduce returns the error of an HTTP answer of status whose body is body,
// for a client that reads the answer itself: the code is the first of the
// body's JSON fields named in codeFields that holds a plain token, and the
// body is read only for whether the request was too long.
func Reduce(provider string, status int, body []byte, codeFields ...string) *Error {
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	code := ""
	for _, name := range codeFields {
		if value, ok := fields[name].(string); ok && Token(value) != "" {
			code = value
			break
		}
	}
	return New(provider, status, code, string(body))
}

// token is what Token keeps: an identifier of letters, digits and _ . - of at
// most 64 characters, as providers' error codes are. A space, as in a
// sentence or a name, makes a code no token.
var token = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\-]{0,63}$`)

// Token returns code when it is a plain token, and "" otherwise.
func Token(code string) string {
	code = strings.TrimSpace(code)
	if !token.MatchString(code) {
		return ""
	}
	return code
}

// tooLongSigns are what providers say of a request longer than the model
// takes: OpenAI's code context_length_exceeded, Anthropic's "prompt is too
// long", Gemini's "exceeds the maximum number of tokens".
var tooLongSigns = []string{
	"context_length_exceeded", "context length", "context window", "maximum context",
	"too many tokens", "token limit", "prompt is too long", "context exceeded",
	"exceeds the maximum number of tokens",
}

func tooLong(text string) bool {
	text = strings.ToLower(text)
	for _, sign := range tooLongSigns {
		if strings.Contains(text, sign) {
			return true
		}
	}
	return false
}
