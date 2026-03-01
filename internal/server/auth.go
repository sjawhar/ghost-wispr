package server

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// BasicAuthMiddleware returns middleware that enforces HTTP Basic auth.
// If token is empty, the middleware is a no-op.
// Username is always "ghost-wispr"; password is the token.
func BasicAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		expected := "ghost-wispr:" + token
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Basic ") {
				unauthorized(w)
				return
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
			if err != nil || subtle.ConstantTimeCompare(decoded, []byte(expected)) != 1 {
				unauthorized(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="ghost-wispr"`)
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}
