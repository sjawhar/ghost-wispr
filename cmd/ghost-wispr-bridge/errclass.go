package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"cloud.google.com/go/auth"
	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// errorClass names err for a log line by a bounded class, never by its text:
// an error's text can repeat what a request or an answer held, such as a
// malformed time, a JSON value or Google's error message (C9, R7). It gives
// status=<code> when err carries an HTTP status, from Attendee or a Google
// API; status=<code> reason=auth for a refused credential, the delegated
// sign-in's or a token endpoint's; and otherwise status=none with a reason:
// timeout, canceled, network, malformed (a JSON or time value that did not
// parse) or other. A refused credential is read before the network check:
// net/http wraps it, as any transport failure, in a *url.Error, which is a
// net.Error.
func errorClass(err error) string {
	var attendee *attendeeStatusError
	if errors.As(err, &attendee) {
		return "status=" + strconv.Itoa(attendee.Status)
	}
	var google *googleapi.Error
	if errors.As(err, &google) {
		return "status=" + strconv.Itoa(google.Code)
	}
	if status, ok := refusedCredential(err); ok {
		return "status=" + status + " reason=auth"
	}
	return "status=none reason=" + errorReason(err)
}

// refusedCredential gives the HTTP status of a refused credential in err,
// "none" when the refusal carries no response.
func refusedCredential(err error) (string, bool) {
	var (
		signIn    *signInError
		retrieve  *oauth2.RetrieveError
		authError *auth.Error
		resp      *http.Response
	)
	switch {
	case errors.As(err, &signIn):
		return strconv.Itoa(signIn.status), true
	case errors.As(err, &authError):
		resp = authError.Response
	case errors.As(err, &retrieve):
		resp = retrieve.Response
	default:
		return "", false
	}
	if resp == nil {
		return "none", true
	}
	return strconv.Itoa(resp.StatusCode), true
}

func errorReason(err error) string {
	var (
		netErr    net.Error
		opErr     *net.OpError
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		timeErr   *time.ParseError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &opErr), errors.As(err, &netErr):
		return "network"
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr), errors.As(err, &timeErr):
		return "malformed"
	default:
		return "other"
	}
}
