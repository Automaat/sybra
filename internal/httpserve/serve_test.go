package httpserve_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/httpserve"
	"github.com/Automaat/sybra/internal/sse"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestWorkerControlRouteRequiresAuthentication(t *testing.T) {
	if !httpserve.RequestRequiresAuth(httptest.NewRequest(http.MethodPost, "/worker/v1/register", http.NoBody)) {
		t.Fatal("worker control route was left outside bearer authentication")
	}
	worker := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	// StaticFS enables the SPA's GET / catch-all. Worker control must coexist
	// with it without triggering a ServeMux pattern-conflict panic.
	handler := httpserve.Handler(httpserve.Options{Logger: testLogger(), WorkerControl: worker, StaticFS: bundle()}, "secret", nil)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/worker/v1/register", http.NoBody))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	authorizedRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/register", http.NoBody)
	authorizedRequest.Header.Set("Authorization", "Bearer secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("authorized status = %d", authorized.Code)
	}
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
		{name: "another local page hiding its referer", remote: "127.0.0.1:5555", fetchSite: "cross-site"},
		{name: "another local page by referer alone", remote: "127.0.0.1:5555", referer: "http://127.0.0.1:9999/"},
		// Neither header means a non-browser caller, which already had to reach
		// loopback and can read the token file anyway.
		{name: "no provenance at all", remote: "127.0.0.1:5555", wantToken: true},
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

// TestHandlerAlwaysSetsTheContentPolicy pins the header as the only source of
// the policy. index.html carried a meta copy whose connect-src 'self' silently
// won over the header, blocking every call an attached window makes.
func TestHandlerAlwaysSetsTheContentPolicy(t *testing.T) {
	opts := httpserve.Options{Logger: testLogger(), StaticFS: bundle()}
	srv := httptest.NewServer(httpserve.Handler(opts, "tok", nil))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "http-equiv=\"Content-Security-Policy\"") {
		t.Fatal("index.html carries a meta policy again; it overrides the header wherever it is stricter")
	}
}

// TestAttachedBoardIsProxiedSameOrigin covers the UI attached to a board on
// another machine. The page must stay same-origin with the instance that served
// it: that board's operator cannot know which loopback port an attaching window
// will pick, so they cannot grant it CORS, and every call would be blocked.
func TestAttachedBoardIsProxiedSameOrigin(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"from-the-board"}`))
	}))
	t.Cleanup(board.Close)

	opts := httpserve.Options{
		Logger:   testLogger(),
		Services: map[string]httpapi.Service{"BrowserService": httpapi.NewService(&stubBrowser{}, "OpenExternal")},
		Proxy:    &httpserve.ProxyTarget{Origin: board.URL, Token: "board-secret"},
		// The bundle too: an attached instance serves both, and registering
		// the proxy without a method conflicts with the static route.
		StaticFS: bundle(),
	}
	srv := httptest.NewServer(httpserve.BuildMux(opts))
	t.Cleanup(srv.Close)

	t.Run("board call is forwarded under the board's own token", func(t *testing.T) {
		resp, err := srv.Client().Post(srv.URL+"/api/InfoService/GetVersion", "application/json", strings.NewReader(`[]`))
		if err != nil {
			t.Fatalf("POST GetVersion: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "from-the-board") {
			t.Fatalf("body = %s, want the board's answer", body)
		}
		if gotAuth != "Bearer board-secret" {
			t.Fatalf("Authorization = %q, want the board's own token", gotAuth)
		}
		if gotPath != "/api/InfoService/GetVersion" {
			t.Fatalf("path = %q", gotPath)
		}
	})

	t.Run("event stream drops the page's query token", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/events?token=local-secret")
		if err != nil {
			t.Fatalf("GET /events: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if strings.Contains(gotQuery, "local-secret") {
			t.Fatalf("query = %q, want this instance's token stripped", gotQuery)
		}
		if gotAuth != "Bearer board-secret" {
			t.Fatalf("Authorization = %q, want the board's own token", gotAuth)
		}
	})

	t.Run("the bundle is still served alongside the proxy", func(t *testing.T) {
		resp, err := srv.Client().Get(srv.URL + "/tasks/abc")
		if err != nil {
			t.Fatalf("GET deep link: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
	})

	t.Run("a host-local service is answered here, not forwarded", func(t *testing.T) {
		gotPath = ""
		resp, err := srv.Client().Post(srv.URL+"/api/BrowserService/OpenExternal", "application/json", strings.NewReader(`["https://example.com"]`))
		if err != nil {
			t.Fatalf("POST OpenExternal: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if gotPath != "" {
			t.Fatalf("reached the board at %q; a host-local action must stay here", gotPath)
		}
	})
}

type stubBrowser struct{}

func (s *stubBrowser) OpenExternal(_ string) error { return nil }

// TestSPARoutingDoesNotProbeTheHost covers the existence check behind the SPA
// fallback. It drives SPAHandler directly rather than through a client,
// because net/http cleans a traversal out of the path before the mux dispatches
// it — the concatenation was still wrong, and SPAHandler is exported, so the
// check answers about the bundle alone now.
func TestSPARoutingDoesNotProbeTheHost(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	outside := filepath.Join(dir, "..", "outside-the-bundle.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	handler, _ := httpserve.BuildMux(httpserve.Options{Logger: testLogger(), StaticDir: dir}).
		Handler(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if handler == nil {
		t.Fatal("no static handler mounted")
	}

	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.URL.Path = "/../outside-the-bundle.txt"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("served a file from outside the bundle: %s", rec.Body.String())
	}
}

// TestAttachedBoardRefusesAnUnusableOrigin keeps a bad target from taking the
// handler goroutine down: a nil parse result would panic inside the proxy on
// the first call rather than answering it.
func TestAttachedBoardRefusesAnUnusableOrigin(t *testing.T) {
	opts := httpserve.Options{
		Logger:   testLogger(),
		Services: map[string]httpapi.Service{},
		Proxy:    &httpserve.ProxyTarget{Origin: "://not-a-url", Token: "t"},
		StaticFS: bundle(),
	}
	srv := httptest.NewServer(httpserve.BuildMux(opts))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Post(srv.URL+"/api/InfoService/GetVersion", "application/json", strings.NewReader(`[]`))
	if err != nil {
		t.Fatalf("POST GetVersion: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// TestAttachedBoardWithABrokerStillStarts pins the one pattern that would panic
// before serving a request: an attached instance takes its events from the
// board it forwards to, so its own broker must not claim /events as well.
func TestAttachedBoardWithABrokerStillStarts(t *testing.T) {
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(board.Close)

	opts := httpserve.Options{
		Logger:   testLogger(),
		Broker:   sse.New(),
		Services: map[string]httpapi.Service{},
		Proxy:    &httpserve.ProxyTarget{Origin: board.URL, Token: "t"},
		StaticFS: bundle(),
	}
	// BuildMux panics on a duplicate pattern, so reaching the request at all is
	// the assertion.
	srv := httptest.NewServer(httpserve.BuildMux(opts))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestHealthDoesNotDiscloseTheHomePath keeps an unauthenticated endpoint from
// handing out the operator's filesystem layout.
//
// The digest is what a client compares to decide whether a board owns this
// disk. The path itself would tell anyone who can reach the port the operator's
// username and data layout — and would let a local process that cannot read the
// home echo it back to collect the bearer token.
func TestHealthDoesNotDiscloseTheHomePath(t *testing.T) {
	home := t.TempDir()
	srv := httptest.NewServer(httpserve.BuildMux(httpserve.Options{Logger: testLogger(), Home: home}))
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), home) {
		t.Fatalf("health disclosed the home path: %s", body)
	}
	if !strings.Contains(string(body), httpserve.HomeID(home)) {
		t.Fatalf("health carries no home digest: %s", body)
	}
}

// TestHomeIDResolvesSymlinks pins the comparison a client makes: a home reached
// through /var and /private/var is one home, and must digest the same.
func TestHomeIDResolvesSymlinks(t *testing.T) {
	actual := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if httpserve.HomeID(actual) != httpserve.HomeID(link) {
		t.Fatal("a home reached through a symlink digests differently")
	}
	if httpserve.HomeID(actual) == httpserve.HomeID(t.TempDir()) {
		t.Fatal("two different homes digest the same")
	}
	if httpserve.HomeID("") != "" {
		t.Fatal("an unset home produced a digest")
	}
}

// TestHomeIDDistinguishesRelativeHomes is a data-loss regression. A relative
// SYBRA_HOME used to digest the bare string, so two processes started from
// different directories agreed they served one home while owning different
// disks — and a cleanup then deleted the other's live state.
func TestHomeIDDistinguishesRelativeHomes(t *testing.T) {
	base := t.TempDir()
	for _, side := range []string{"relA", "relB"} {
		if err := os.MkdirAll(filepath.Join(base, side, "myhome"), 0o755); err != nil {
			t.Fatalf("seed %s: %v", side, err)
		}
	}

	digest := func(dir string) string {
		t.Helper()
		t.Chdir(dir)
		return httpserve.HomeID("myhome")
	}
	a := digest(filepath.Join(base, "relA"))
	b := digest(filepath.Join(base, "relB"))

	if a == "" || b == "" {
		t.Fatal("a relative home produced no digest")
	}
	if a == b {
		t.Fatal("two different directories reached by the same relative home digest the same")
	}
	// And the relative form still matches its own absolute form.
	t.Chdir(filepath.Join(base, "relA"))
	if httpserve.HomeID("myhome") != httpserve.HomeID(filepath.Join(base, "relA", "myhome")) {
		t.Fatal("a relative home does not match the absolute path it names")
	}
}
