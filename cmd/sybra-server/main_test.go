package main

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sse"
	"github.com/Automaat/sybra/internal/sybra"
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

func TestAuthMiddlewareAllowsStaticRouteWithoutToken(t *testing.T) {
	h := authMiddleware("secret", testLogger(), okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
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

func TestCorsMiddlewarePreservesExistingVaryHeaders(t *testing.T) {
	h := corsMiddleware([]string{"https://allowed.example"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/health", http.NoBody)
	req.Header.Set("Origin", "https://allowed.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	got := rr.Result().Header.Values("Vary")
	if len(got) != 2 || got[0] != "Origin" || got[1] != "Accept-Encoding" {
		t.Fatalf("Vary headers = %q, want [Origin Accept-Encoding]", got)
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

func TestServerHandlerServesSPAWithoutToken(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYBRA_STATIC_DIR", staticDir)

	cfg := config.DefaultConfig()
	cfg.Server.AuthToken = "secret"

	app := sybra.NewApp(testLogger(), nil, cfg)
	handler := cspMiddleware(corsMiddleware(nil, authMiddleware(cfg.Server.AuthToken, testLogger(), buildMux(testLogger(), sse.New(), app))))

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

func TestSSEHandlerNeverSetsWildcardCORS(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.AuthToken = "secret"
	cfg.Server.AllowedOrigins = []string{"https://allowed.example"}

	app := sybra.NewApp(testLogger(), nil, cfg)
	handler := cspMiddleware(corsMiddleware(cfg.Server.AllowedOrigins, authMiddleware(cfg.Server.AuthToken, testLogger(), buildMux(testLogger(), sse.New(), app))))

	for _, path := range []string{"/events?token=secret", "/api/events/task:updated?token=secret"} {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody).WithContext(ctx)
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			handler.ServeHTTP(rr, req)
			close(done)
		}()
		<-time.After(20 * time.Millisecond)
		cancel()
		<-done

		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want empty for an unlisted origin", path, got)
		}
	}
}

func TestSPAHandlerFallsBackToIndexForUnknownRoute(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(os.DirFS(staticDir), ".")
	if err != nil {
		t.Fatal(err)
	}
	h := spaHandler{fs: http.FileServer(http.FS(sub)), staticDir: staticDir}

	req := httptest.NewRequest(http.MethodGet, "/tasks/123", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); body != "index" {
		t.Fatalf("body = %q, want %q", body, "index")
	}
}
