package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"

	"google.golang.org/api/googleapi"
)

// errorClass names err for a log line by a bounded class, never by its text:
// an error's text can repeat what a request or an answer held, such as a
// malformed time, a JSON value or Google's error message (C9, R7). It gives
// status=<code> when err carries an HTTP status, from Attendee or a Google
// API, and otherwise status=none with a reason: timeout, canceled, network,
// malformed (a JSON or time value that did not parse) or other.
func errorClass(err error) string {
	var attendee *attendeeStatusError
	if errors.As(err, &attendee) {
		return "status=" + strconv.Itoa(attendee.Status)
	}
	var google *googleapi.Error
	if errors.As(err, &google) {
		return "status=" + strconv.Itoa(google.Code)
	}
	return "status=none reason=" + errorReason(err)
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
