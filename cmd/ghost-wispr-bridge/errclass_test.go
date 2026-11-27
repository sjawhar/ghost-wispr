package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// errclassSentinel stands for words of a meeting inside an error's text.
const errclassSentinel = "zebra-sentinel-4417"

// A log line names an error by a bounded class: an HTTP status or a fixed
// reason, never the error's text.
func TestAnErrorIsLoggedByItsClassAlone(t *testing.T) {
	_, timeErr := time.Parse(time.RFC3339, errclassSentinel)
	jsonErr := json.Unmarshal([]byte(`{"x":`+errclassSentinel+`}`), &struct{}{})
	typeErr := json.Unmarshal([]byte(`{"x":"`+errclassSentinel+`"}`), &struct{ X int }{})
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"Attendee's status", fmt.Errorf("read %s: %w", errclassSentinel, &attendeeStatusError{Method: "GET", Path: "/bots/" + errclassSentinel, Status: 503}), "status=503"},
		{"Google's status", fmt.Errorf("list: %w", &googleapi.Error{Code: 403, Message: errclassSentinel, Body: errclassSentinel}), "status=403"},
		{"a deadline", fmt.Errorf("%s: %w", errclassSentinel, context.DeadlineExceeded), "status=none reason=timeout"},
		{"a network timeout", &url.Error{Op: "Get", URL: "http://x/" + errclassSentinel, Err: &net.OpError{Op: "dial", Err: errclassTimeout{}}}, "status=none reason=timeout"},
		{"a cancel", fmt.Errorf("%s: %w", errclassSentinel, context.Canceled), "status=none reason=canceled"},
		{"a refused connection", &url.Error{Op: "Post", URL: "http://x/" + errclassSentinel, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(errclassSentinel)}}, "status=none reason=network"},
		{"a malformed time", fmt.Errorf("event: read its start: %w", timeErr), "status=none reason=malformed"},
		{"a malformed JSON body", fmt.Errorf("attendee: GET /bots/%s: %w", errclassSentinel, jsonErr), "status=none reason=malformed"},
		{"a JSON value of another type", fmt.Errorf("attendee: GET /bots/%s: %w", errclassSentinel, typeErr), "status=none reason=malformed"},
		{"any other error", errors.New(errclassSentinel), "status=none reason=other"},
		{"an HTTP status beside a deadline", fmt.Errorf("%s %w: %w", errclassSentinel, &attendeeStatusError{Status: http.StatusBadRequest}, context.DeadlineExceeded), "status=400"},
		// A refused credential comes inside the *url.Error net/http makes of
		// any transport failure, which is a net.Error: it is a sign-in's
		// refusal all the same, not a network failure.
		{"a token endpoint's refusal", &url.Error{Op: "Get", URL: "https://x/" + errclassSentinel,
			Err: &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusBadRequest}, Body: []byte(errclassSentinel), ErrorCode: "invalid_grant"}},
			"status=400 reason=auth"},
		{"a token provider's refusal", &url.Error{Op: "Get", URL: "https://x/" + errclassSentinel,
			Err: &auth.Error{Response: &http.Response{StatusCode: http.StatusUnauthorized}, Body: []byte(errclassSentinel)}},
			"status=401 reason=auth"},
		{"a refusal with no response", &url.Error{Op: "Get", URL: "https://x/" + errclassSentinel, Err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}},
			"status=none reason=auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.err.Error(), errclassSentinel) {
				t.Fatalf("the case's error %q does not hold the sentinel, so it cannot show the class leaves it out", tc.err)
			}
			got := errorClass(tc.err)
			if got != tc.want {
				t.Fatalf("errorClass = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, errclassSentinel) {
				t.Fatalf("errorClass %q holds the error's text", got)
			}
		})
	}
}

type errclassTimeout struct{}

func (errclassTimeout) Error() string   { return "i/o timeout " + errclassSentinel }
func (errclassTimeout) Timeout() bool   { return true }
func (errclassTimeout) Temporary() bool { return true }
