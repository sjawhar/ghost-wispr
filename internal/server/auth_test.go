package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuthMiddleware_NoToken_PassThrough(t *testing.T) {
	middleware := BasicAuthMiddleware("")
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("expected body 'ok', got %q", rr.Body.String())
	}
}

func TestBasicAuthMiddleware_CorrectPassword_OK(t *testing.T) {
	token := "secret123"
	middleware := BasicAuthMiddleware(token)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authenticated"))
	}))

	// Create correct auth header
	credentials := "ghost-wispr:" + token
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic "+encoded)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if rr.Body.String() != "authenticated" {
		t.Fatalf("expected body 'authenticated', got %q", rr.Body.String())
	}
}

func TestBasicAuthMiddleware_WrongPassword_401(t *testing.T) {
	token := "secret123"
	middleware := BasicAuthMiddleware(token)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Create wrong auth header
	credentials := "ghost-wispr:wrongpassword"
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic "+encoded)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") != `Basic realm="ghost-wispr"` {
		t.Fatalf("expected WWW-Authenticate header, got %q", rr.Header().Get("WWW-Authenticate"))
	}
}

func TestBasicAuthMiddleware_MissingHeader_401(t *testing.T) {
	token := "secret123"
	middleware := BasicAuthMiddleware(token)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No Authorization header
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", rr.Code)
	}
	if rr.Header().Get("WWW-Authenticate") != `Basic realm="ghost-wispr"` {
		t.Fatalf("expected WWW-Authenticate header, got %q", rr.Header().Get("WWW-Authenticate"))
	}
}
