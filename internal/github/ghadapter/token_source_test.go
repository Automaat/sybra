package ghadapter

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

// fakeAppServer stands in for api.github.com's App-auth endpoints. mints
// counts installation-token requests so tests can assert on collapsing.
type fakeAppServer struct {
	mu      sync.Mutex
	mints   int32
	ttl     time.Duration
	slug    string
	fail401 bool
}

func newFakeAppServer(t *testing.T) (*fakeAppServer, *httptest.Server) {
	t.Helper()
	f := &fakeAppServer{ttl: time.Hour, slug: "sybra-app"}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"message":"missing jwt"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		fail := f.fail401
		f.mu.Unlock()
		if fail {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&f.mints, 1)
		f.mu.Lock()
		ttl := f.ttl
		f.mu.Unlock()
		resp := struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
		}{
			Token:     fmt.Sprintf("tok-%d", atomic.LoadInt32(&f.mints)),
			ExpiresAt: time.Now().Add(ttl).UTC().Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/app", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, `{"message":"missing jwt"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		slug := f.slug
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Slug string `json:"slug"`
		}{Slug: slug})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestNewTokenSource_RejectsNonPositiveIDs(t *testing.T) {
	key := testPrivateKeyPEM(t)
	cases := []struct {
		name           string
		appID          int64
		installationID int64
	}{
		{"zero app id", 0, 2},
		{"zero installation id", 1, 0},
		{"negative app id", -1, 2},
		{"negative installation id", 1, -2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTokenSource(tc.appID, tc.installationID, key); err == nil {
				t.Fatalf("NewTokenSource(%d, %d): want error, got nil", tc.appID, tc.installationID)
			}
		})
	}
}

func newTestSource(t *testing.T, srv *httptest.Server) *TokenSource {
	t.Helper()
	orig := ghAPIBaseURL
	ghAPIBaseURL = srv.URL
	t.Cleanup(func() { ghAPIBaseURL = orig })

	src, err := NewTokenSource(1, 2, testPrivateKeyPEM(t))
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}
	return src
}

// Startup readiness: constructing a TokenSource performs no I/O, so Cached()
// must report empty before any Refresh/ForceRefresh call.
func TestCached_EmptyBeforeFirstRefresh(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	if got := src.Cached(); got != "" {
		t.Fatalf("Cached() before any refresh = %q, want empty", got)
	}
	if atomic.LoadInt32(&f.mints) != 0 {
		t.Fatalf("Cached() minted a token: %d mints, want 0", f.mints)
	}
}

func TestRefresh_ThenCachedIsNonblocking(t *testing.T) {
	_, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := src.Cached()
	if got == "" {
		t.Fatalf("Cached() after Refresh = empty, want a token")
	}
}

// Concurrent refresh collapse: N goroutines calling Refresh against an
// expired/unset token must result in exactly one mint, same guarantee
// appTokenSource.refresh gives via its singleflight channel — here it comes
// from ghinstallation.Transport's own mutex around isExpired+refreshToken.
func TestRefresh_ConcurrentCollapsesToOneMint(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			errs[i] = src.Refresh(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Refresh[%d]: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&f.mints); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
}

// ForceRefresh must collapse concurrent callers the same way — this is the
// behavior ghinstallation.Transport does NOT provide out of the box (its
// mutex only collapses around isExpired(), not an unconditional force), so
// ForceRefresh reimplements singleflight explicitly.
func TestForceRefresh_ConcurrentCollapsesToOneMint(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	atomic.StoreInt32(&f.mints, 0)

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			errs[i] = src.ForceRefresh(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ForceRefresh[%d]: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&f.mints); got != 1 {
		t.Fatalf("mints = %d, want 1", got)
	}
}

// ForceRefresh must mint even when the cached token is nowhere near expiry —
// the scenario a plain Refresh() would no-op against at the hourly rotation
// boundary (#2453).
func TestForceRefresh_MintsEvenWhenCachedIsFresh(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	if got := atomic.LoadInt32(&f.mints); got != 1 {
		t.Fatalf("seed mints = %d, want 1", got)
	}

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if got := atomic.LoadInt32(&f.mints); got != 1 {
		t.Fatalf("Refresh against a fresh token minted: %d mints, want 1", got)
	}

	if err := src.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh: %v", err)
	}
	if got := atomic.LoadInt32(&f.mints); got != 2 {
		t.Fatalf("ForceRefresh mints = %d, want 2", got)
	}
}

// Token-expiry-driven renewal: Cached() must stop returning a token once it
// falls inside the renew window, and Refresh() must mint a new one.
func TestExpiry_CachedGoesEmptyAndRefreshRenews(t *testing.T) {
	f, srv := newFakeAppServer(t)
	f.mu.Lock()
	f.ttl = 30 * time.Second // inside ghinstallation's 1-minute refresh window
	f.mu.Unlock()
	src := newTestSource(t, srv)

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := src.Cached(); got != "" {
		t.Fatalf("Cached() with a near-expiry token = %q, want empty", got)
	}

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh after near-expiry: %v", err)
	}
	if got := atomic.LoadInt32(&f.mints); got != 2 {
		t.Fatalf("mints after renewal = %d, want 2", got)
	}
}

// Cached() must never reach into the ghinstallation.Transport. Doing so broke
// the nonblocking invariant two ways: Expiry() reads the Transport's cached
// token without the library mutex Token() writes it under (a data race this
// test surfaces under -race), and a token crossing refreshAt between that
// check and the following Token() call made the "nonblocking" read mint
// synchronously. With ttl inside ghinstallation's renew window every Refresh
// re-mints, so the reader races a continuous stream of Transport writes and
// must still observe only "" — never a stale token, never a mint of its own.
func TestCached_NeverTouchesTransportDuringConcurrentRefresh(t *testing.T) {
	f, srv := newFakeAppServer(t)
	f.mu.Lock()
	f.ttl = 30 * time.Second // always inside the 1-minute refresh window
	f.mu.Unlock()
	src := newTestSource(t, srv)

	const refreshes = 20
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !stop.Load() {
			if got := src.Cached(); got != "" {
				t.Errorf("Cached() during refresh = %q, want empty (token is inside the renew window)", got)
				return
			}
		}
	})

	for i := range refreshes {
		if err := src.Refresh(context.Background()); err != nil {
			t.Errorf("Refresh[%d]: %v", i, err)
			break
		}
	}
	stop.Store(true)
	wg.Wait()

	// Every Refresh mints (token is always stale) and Cached() adds none.
	if got := atomic.LoadInt32(&f.mints); got != refreshes {
		t.Fatalf("mints = %d, want %d — Cached() minted on the nonblocking path", got, refreshes)
	}
}

// A failed renewal must not blank an already-cached token: the previously
// minted one stays valid until its own expiry, and GHEnv() should keep using
// it rather than fall back to ambient auth on a transient mint failure.
func TestRefresh_FailureKeepsPreviouslyCachedToken(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}
	seeded := src.Cached()
	if seeded == "" {
		t.Fatalf("Cached() after seed Refresh = empty, want a token")
	}

	f.mu.Lock()
	f.fail401 = true
	f.mu.Unlock()

	if err := src.ForceRefresh(context.Background()); err == nil {
		t.Fatalf("ForceRefresh during simulated 401 = nil error, want failure")
	}
	if got := src.Cached(); got != seeded {
		t.Fatalf("Cached() after a failed refresh = %q, want the seeded %q", got, seeded)
	}
}

// 401-triggered recovery: simulate the mint endpoint rejecting the App JWT,
// then recovering, and confirm ForceRefresh surfaces the failure and later
// succeeds once the fake server heals — the shape of appauth.go's
// onAuthFailureObserved -> ForceRefreshAppToken path.
func Test401Recovery_ForceRefreshSurfacesThenHeals(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	f.mu.Lock()
	f.fail401 = true
	f.mu.Unlock()

	if err := src.ForceRefresh(context.Background()); err == nil {
		t.Fatalf("ForceRefresh during simulated 401 = nil error, want failure")
	}

	f.mu.Lock()
	f.fail401 = false
	f.mu.Unlock()

	if err := src.ForceRefresh(context.Background()); err != nil {
		t.Fatalf("ForceRefresh after recovery: %v", err)
	}
	if got := src.Cached(); got == "" {
		t.Fatalf("Cached() after recovered ForceRefresh = empty, want a token")
	}
}

func TestAppLogin_FetchesAndCachesSlug(t *testing.T) {
	f, srv := newFakeAppServer(t)
	src := newTestSource(t, srv)

	login, err := src.AppLogin(context.Background())
	if err != nil {
		t.Fatalf("AppLogin: %v", err)
	}
	if login != "sybra-app[bot]" {
		t.Fatalf("AppLogin = %q, want sybra-app[bot]", login)
	}

	// Change the fake server's slug; a cached AppLogin must not re-fetch —
	// mirrors appTokenSource.appLogin caching the slug for the process
	// lifetime since it's immutable for the App's lifetime.
	f.mu.Lock()
	f.slug = "renamed-app"
	f.mu.Unlock()

	login2, err := src.AppLogin(context.Background())
	if err != nil {
		t.Fatalf("AppLogin (cached): %v", err)
	}
	if login2 != "sybra-app[bot]" {
		t.Fatalf("AppLogin (cached) = %q, want sybra-app[bot] (unchanged)", login2)
	}
}
