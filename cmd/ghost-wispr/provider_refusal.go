package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/ghost-wispr/internal/providererr"
)

// providerRefusal is err's refusal by a provider that no retry mends: an
// answer with an HTTP status that is not a rate limit or a server fault.
func providerRefusal(err error) (*providererr.Error, bool) {
	var refusal *providererr.Error
	if errors.As(err, &refusal) && refusal.Status != 0 && !refusal.Retryable() {
		return refusal, true
	}
	return nil, false
}

// refusalWarning says, for a status page warning, how a provider refused a
// call of a role's model: who answered, its status and code, never its
// message, which can quote what it was sent; then what to check. A 401
// refused the sign-in; a 403 answered a sign-in that worked, refusing this
// model to it (a gateway's team_model_access_denied); anything else is
// modelCheck.
func refusalWarning(refusal *providererr.Error, modelCheck string) string {
	answer := fmt.Sprintf("%s answered HTTP %d", refusal.Provider, refusal.Status)
	if refusal.Code != "" {
		answer += " (" + refusal.Code + ")"
	}
	switch refusal.Status {
	case http.StatusUnauthorized:
		return answer + "; check its provider's sign-in under Model providers"
	case http.StatusForbidden:
		return answer + "; the provider refused this model for this sign-in"
	}
	return answer + "; " + modelCheck
}
