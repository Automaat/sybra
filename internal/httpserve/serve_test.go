package httpserve_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Automaat/sybra/internal/httpserve"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func bundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":     &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/app.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon-16.png": &fstest.MapFile{Data: []byte("png")},
	}
}

// TestEmbeddedSPARouting covers the bundle path the desktop app serves: a deep
// link has to reach index.html, while a missing asset must stay a 404 rather
// than becoming HTML the browser reports as a broken image.
func TestEmbeddedSPARouting(t *testing.T) {
	mux := httpserve.BuildMux(httpserve.Options{Logger: testLogger(), StaticFS: bundle()})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "root", path: "/", wantStatus: http.StatusOK, wantBody: "<html>app</html>"},
		{name: "deep link", path: "/tasks/abc123", wantStatus: http.StatusOK, wantBody: "<html>app</html>"},
		{name: "encoded segment", path: "/projects/owner%2Frepo", wantStatus: http.StatusOK, wantBody: "<html>app</html>"},
		{name: "present asset", path: "/assets/app.js", wantStatus: http.StatusOK, wantBody: "console.log(1)"},
		{name: "missing asset", path: "/missing.png", wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantBody == "" {
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if got := strings.TrimSpace(string(body)); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// TestRuntimeConfigDisclosesTokenToLoopbackOnly is the guard on the one
// response that carries the bearer token: the desktop window reads it over
// loopback, and anything arriving through a proxy must get the origin alone.
func TestRuntimeConfigDisclosesTokenToLoopbackOnly(t *testing.T) {
	const self = "http://127.0.0.1:1234"
	opts := httpserve.Options{
		Logger:     testLogger(),
		APIBase:    self + "/api",
		Token:      "s3cret",
		SelfOrigin: self,
	}
	mux := httpserve.BuildMux(opts)

	tests := []struct {
		name      string
		remote    string
		forwarded string
		fetchSite string
		referer   string
		wantToken bool
	}{
		{name: "same origin page", remote: "127.0.0.1:5555", fetchSite: "same-origin", wantToken: true},
		{name: "same origin v6", remote: "[::1]:5555", fetchSite: "same-origin", wantToken: true},
		{name: "referer fallback", remote: "127.0.0.1:5555", referer: self + "/tasks", wantToken: true},
		{name: "another local page", remote: "127.0.0.1:5555", fetchSite: "cross-site", referer: "https://evil.example/"},
		{name: "no provenance at all", remote: "127.0.0.1:5555"},
		{name: "lan", remote: "192.168.20.5:5555", fetchSite: "same-origin"},
		{name: "proxied through loopback", remote: "127.0.0.1:5555", forwarded: "203.0.113.9", fetchSite: "same-origin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/runtime-config.js", http.NoBody)
			req.RemoteAddr = tc.remote
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if tc.referer != "" {
				req.Header.Set("Referer", tc.referer)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"apiBase":"`+self+`/api"`) {
				t.Fatalf("body missing apiBase: %s", body)
			}
			if got := strings.Contains(body, "s3cret"); got != tc.wantToken {
				t.Fatalf("token disclosed = %v, want %v (body %s)", got, tc.wantToken, body)
			}
		})
	}
}

// TestRuntimeConfigIsUnauthenticated keeps the bootstrap reachable: the UI has
// no token until it reads this, so requiring one would deadlock the desktop
// window against its own server.
func TestRuntimeConfigIsUnauthenticated(t *testing.T) {
	opts := httpserve.Options{Logger: testLogger(), APIBase: "/api", Token: "s3cret"}
	srv := httptest.NewServer(httpserve.Handler(opts, "s3cret", nil))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/runtime-config.js")
	if err != nil {
		t.Fatalf("GET runtime-config.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestCSPWidensConnectSrcForAnAttachedBoard covers the UI attached elsewhere:
// its own origin serves the bundle, so without the board's origin in the policy
// every call it makes is blocked before it leaves the page.
func TestCSPWidensConnectSrcForAnAttachedBoard(t *testing.T) {
	tests := []struct {
		name          string
		connectOrigin string
		want          string
	}{
		{name: "own board", want: "connect-src 'self' ws: wss:;"},
		{name: "attached board", connectOrigin: "https://board.example", want: "connect-src 'self' ws: wss: https://board.example;"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpserve.CSPMiddleware(tc.connectOrigin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
			if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, tc.want) {
				t.Fatalf("policy = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}
