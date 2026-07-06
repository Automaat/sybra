package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthMiddlewareRejectsMissingToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuthMiddlewareAcceptsMatchingBearerToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuthMiddlewareRejectsWrongBearerToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestAuthMiddlewareAllowsHealthWithoutToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestAuthMiddlewareBlankTokenFailsClosed(t *testing.T) {
	h := authMiddleware("", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Authorization", "Bearer ")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (blank server token must never authorize)", rr.Code)
	}
}

func TestAuthMiddlewareSSEAcceptsQueryToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/events?token=secret", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE path should accept query token)", rr.Code)
	}
}

func TestAuthMiddlewareNonSSEPathRejectsQueryToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/ListTasks?token=secret", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (query token must only be honored on SSE paths)", rr.Code)
	}
}

func TestCorsMiddlewareEchoesAllowedOrigin(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the allowed origin", got)
	}
}

func TestCorsMiddlewareOmitsHeadersForUnlistedOrigin(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for an unlisted origin", got)
	}
}

func TestCorsMiddlewareNeverSetsWildcard(t *testing.T) {
	h := corsMiddleware(nil, okHandler())
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://anything.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("Access-Control-Allow-Origin must never be a wildcard")
	}
}

func TestCorsMiddlewareHandlesPreflight(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, okHandler())
	req := httptest.NewRequest(http.MethodOptions, "/api/TaskService/ListTasks", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 for OPTIONS preflight", rr.Code)
	}
}
