package main

import (
	"context"
	"fmt"

	"cloud.google.com/go/compute/metadata"
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
	svc, err := calendarapi.NewService(ctx, append([]option.ClientOption{option.WithTokenSource(ts)}, cal...)...)
	if err != nil {
		return nil, fmt.Errorf("calendar client: %w", err)
	}
	return svc, nil
}
