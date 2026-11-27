package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	calendarapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

// delegatedCalendar gives a Calendar API client that signs in as botUser by
// domain-wide delegation, for scope (plan D18). The instance's own service
// account, which the metadata server names, signs the token request's JWT
// with the IAM Credentials API's signJwt on itself, and Google's token
// endpoint exchanges it for botUser's token: no key is stored anywhere. The
// account needs signJwt on itself and, in the bot organization's admin
// console, domain-wide delegation for scope.
func delegatedCalendar(ctx context.Context, scope, botUser string) (*calendarapi.Service, error) {
	return delegatedCalendarVia(ctx, metadata.NewClient(nil), scope, botUser, nil, nil)
}

// delegatedCalendarVia is delegatedCalendar with the metadata server's
// client, the sign-in's client options (the base credentials that call
// signJwt; Application Default Credentials when none) and the Calendar
// client's options given.
func delegatedCalendarVia(ctx context.Context, md *metadata.Client, scope, botUser string, signIn, cal []option.ClientOption) (*calendarapi.Service, error) {
	account, err := md.EmailWithContext(ctx, "default")
	if err != nil {
		return nil, fmt.Errorf("read the instance's service account from the metadata server: %w", err)
	}
	ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: account,
		Scopes:          []string{scope},
		Subject:         botUser,
	}, signIn...)
	if err != nil {
		return nil, fmt.Errorf("delegated sign-in as the bot user through %s: %w", account, err)
	}
	svc, err := calendarapi.NewService(ctx, append([]option.ClientOption{option.WithTokenSource(signInSource{ts})}, cal...)...)
	if err != nil {
		return nil, fmt.Errorf("calendar client: %w", err)
	}
	return svc, nil
}

// signInError is a delegated sign-in that Google refused with an HTTP
// status, at signJwt or at the token exchange: a missing permission or a
// missing domain-wide delegation, which errorClass logs as reason=auth.
type signInError struct {
	status int
	err    error
}

func (e *signInError) Error() string { return e.err.Error() }
func (e *signInError) Unwrap() error { return e.err }

// signInSource is the impersonated token source with its refusals typed.
// The impersonate package reports a refused signJwt or token exchange only
// as text, "impersonate: status code <n>: <body>" (google.golang.org/api
// v0.287.1, impersonate/user.go:131-132,156-157 and impersonate.go:193-194);
// TestARefusedDelegatedSignInIsLoggedAsAuth fails if that text changes.
type signInSource struct{ ts oauth2.TokenSource }

func (s signInSource) Token() (*oauth2.Token, error) {
	tok, err := s.ts.Token()
	if err == nil {
		return tok, nil
	}
	rest, ok := strings.CutPrefix(err.Error(), "impersonate: status code ")
	if !ok {
		return nil, err
	}
	code, _, _ := strings.Cut(rest, ":")
	status, perr := strconv.Atoi(code)
	if perr != nil {
		return nil, err
	}
	return nil, &signInError{status: status, err: err}
}
