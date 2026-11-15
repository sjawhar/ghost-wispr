package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/logging"
)

// originPolicy keeps pages on other sites from using the unit through a
// tailnet member's browser. The unit has no login, so a page anywhere on the
// internet could otherwise open /ws and read the live transcript, or send a
// POST that ends someone's pause.
//
// A request from a browser page names the page's origin in its Origin header:
// always for a WebSocket handshake, and for every POST, PATCH, PUT and DELETE.
// The unit accepts the host the request came to and its own tailnet HTTPS
// address (config public_url), and refuses every other origin, "null"
// included. A request with no Origin header is not a browser page's (curl, an
// agent, the acceptance scripts) and is accepted.
type originPolicy struct {
	cfg    *config.Store
	logger *slog.Logger
}

// allows reports whether r carries no Origin header or names one of the
// unit's own origins.
func (p originPolicy) allows(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) > 1 {
		return false
	}
	origin, ok := parseOrigin(values[0])
	if !ok {
		return false
	}
	if canonicalHost(origin.Host, origin.Scheme) == canonicalHost(r.Host, "") {
		return true
	}
	public := p.publicOrigin()
	return public != nil && origin.Scheme == public.Scheme &&
		canonicalHost(origin.Host, origin.Scheme) == canonicalHost(public.Host, public.Scheme)
}

// publicOrigin is the unit's own tailnet HTTPS address, nil when none is set.
func (p originPolicy) publicOrigin() *url.URL {
	if p.cfg == nil {
		return nil
	}
	public, ok := parseOrigin(p.cfg.Get().PublicURL)
	if !ok {
		return nil
	}
	return public
}

// guard refuses, with 403, a request from another site's page that opens the
// WebSocket (GET /ws) or can change something (any method but GET, HEAD and
// OPTIONS).
func (p originPolicy) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checksOrigin(r) && !p.allows(r) {
			logging.FromContext(r.Context(), p.logger).Warn("refused a request from another origin",
				"operation", "check_origin", "method", r.Method, "path", r.URL.Path, "origin", r.Header.Values("Origin"))
			writeJSONError(w, http.StatusForbidden, "request origin is not this unit")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func checksOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet:
		return r.URL.Path == "/ws"
	case http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// parseOrigin reads a serialized origin, scheme://host[:port] with an http or
// https scheme and nothing after the host, as browsers send it. A trailing
// slash is allowed, so a configured address may be written with one.
func parseOrigin(raw string) (*url.URL, bool) {
	origin, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || origin.Host == "" || origin.User != nil || origin.Opaque != "" ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, false
	}
	if origin.Scheme != "http" && origin.Scheme != "https" {
		return nil, false
	}
	return origin, true
}

// canonicalHost is host in lower case without its scheme's default port. With
// no scheme (a request's Host header, whose scheme a proxy in front may have
// changed), both default ports are dropped.
func canonicalHost(host, scheme string) string {
	host = strings.ToLower(host)
	switch scheme {
	case "https":
		return strings.TrimSuffix(host, ":443")
	case "http":
		return strings.TrimSuffix(host, ":80")
	default:
		return strings.TrimSuffix(strings.TrimSuffix(host, ":443"), ":80")
	}
}
