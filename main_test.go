//go:build darwin

package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Automaat/sybra/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestDesktopBrowserOptions(t *testing.T) {
	t.Parallel()

	truthy, falsy := true, false

	cases := []struct {
		name    string
		cfg     *config.Config
		wantLen int
	}{
		{"default/nil config wires the opener", nil, 1},
		{"nil field wires the opener", &config.Config{}, 1},
		{"explicit true wires the opener", &config.Config{Browser: config.BrowserConfig{InApp: &truthy}}, 1},
		{"explicit false omits the opener", &config.Config{Browser: config.BrowserConfig{InApp: &falsy}}, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := desktopBrowserOptions(tc.cfg, func(string) {})
			if len(opts) != tc.wantLen {
				t.Errorf("desktopBrowserOptions() returned %d options, want %d", len(opts), tc.wantLen)
			}
		})
	}
}

func TestPprofAuthMiddlewareRejectsMissingToken(t *testing.T) {
	t.Parallel()

	h := pprofAuthMiddleware("secret", testLogger(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestPprofAuthMiddlewareAcceptsMatchingBearerToken(t *testing.T) {
	t.Parallel()

	h := pprofAuthMiddleware("secret", testLogger(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestPprofAuthMiddlewareBlankTokenFailsClosed(t *testing.T) {
	t.Parallel()

	h := pprofAuthMiddleware("", testLogger(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody)
	req.Header.Set("Authorization", "Bearer ")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (blank server token must never authorize)", rr.Code)
	}
}
