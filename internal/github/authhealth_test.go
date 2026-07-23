package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthHealthSnapshot_DefaultHealthy(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	snap := AuthHealthSnapshot()
	if snap.State != AuthHealthy {
		t.Fatalf("State = %q, want healthy", snap.State)
	}
	if open, _ := AuthCircuitOpen(); open {
		t.Fatal("circuit should not be open by default")
	}
}

func TestObserveCallResult_NonAuthErrorLeavesStateUnchanged(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	ObserveCallResult([]byte("HTTP 502 Bad Gateway"), fmt.Errorf("exit status 1"))
	if snap := AuthHealthSnapshot(); snap.State != AuthHealthy {
		t.Fatalf("State = %q, want healthy (non-auth error must not change state)", snap.State)
	}
}

func TestObserveCallResult_Success_ResetsCircuit(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	t.Cleanup(DisableAppAuth)

	// No App auth configured: an observed auth failure goes straight to
	// unavailable (nothing to force-refresh).
	ObserveCallResult(nil, fmt.Errorf("gh: HTTP 401: Bad credentials"))
	if snap := AuthHealthSnapshot(); snap.State != AuthUnavailable {
		t.Fatalf("State after failure = %q, want unavailable", snap.State)
	}
	if open, _ := AuthCircuitOpen(); !open {
		t.Fatal("circuit should be open after an unavailable classification")
	}

	ObserveCallResult([]byte("ok"), nil)
	if snap := AuthHealthSnapshot(); snap.State != AuthHealthy {
		t.Fatalf("State after success = %q, want healthy", snap.State)
	}
	if open, _ := AuthCircuitOpen(); open {
		t.Fatal("circuit should close after an observed success")
	}
}

func TestObserveCallResult_NoAppAuth_SetsUnavailable(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	t.Cleanup(DisableAppAuth)
	DisableAppAuth()

	ObserveCallResult(nil, fmt.Errorf("gh: authentication required, run `gh auth login`"))

	snap := AuthHealthSnapshot()
	if snap.State != AuthUnavailable {
		t.Fatalf("State = %q, want unavailable", snap.State)
	}
	if snap.ConsecutiveFailures != 1 {
		t.Fatalf("ConsecutiveFailures = %d, want 1", snap.ConsecutiveFailures)
	}
}

func TestObserveCallResult_AppAuthConfigured_RefreshSucceeds_SetsHealthy(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	t.Cleanup(DisableAppAuth)

	path, _ := writeTestKey(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_recovered","expires_at":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	defer srv.Close()
	origBase := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = origBase })

	if err := EnableAppAuth(AppCredentials{AppID: 1, InstallationID: 2, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	resetAuthHealthForTest() // EnableAppAuth resets viewer cache only; keep health clean.

	ObserveCallResult(nil, fmt.Errorf("gh: HTTP 401: Bad credentials"))

	snap := AuthHealthSnapshot()
	if snap.State != AuthHealthy {
		t.Fatalf("State = %q, want healthy after a successful force-refresh", snap.State)
	}
	if got := CurrentAppToken(); got != "ghs_recovered" {
		t.Fatalf("CurrentAppToken() = %q, want the freshly minted token", got)
	}
}

func TestObserveCallResult_AppAuthConfigured_RefreshFails_ClassifiesMisconfigured(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	t.Cleanup(DisableAppAuth)

	path, _ := writeTestKey(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()
	origBase := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = origBase })

	if err := EnableAppAuth(AppCredentials{AppID: 1, InstallationID: 2, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	resetAuthHealthForTest()

	ObserveCallResult(nil, fmt.Errorf("gh: HTTP 401: Bad credentials"))

	snap := AuthHealthSnapshot()
	if snap.State != AuthMisconfigured {
		t.Fatalf("State = %q, want misconfigured (mint 401 is a permanent credential error)", snap.State)
	}
}

func TestObserveCallResult_AppAuthConfigured_MintNetworkFailure_ClassifiesUnavailable(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()
	t.Cleanup(DisableAppAuth)

	path, _ := writeTestKey(t, false)
	if err := EnableAppAuth(AppCredentials{AppID: 1, InstallationID: 2, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	origBase := appAPIBaseURL
	// An address nothing listens on: the client.Do call fails before any
	// HTTP response, which is the transient (no verdict from GitHub) case.
	appAPIBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { appAPIBaseURL = origBase })
	resetAuthHealthForTest()

	ObserveCallResult(nil, fmt.Errorf("gh: HTTP 401: Bad credentials"))

	snap := AuthHealthSnapshot()
	if snap.State != AuthUnavailable {
		t.Fatalf("State = %q, want unavailable (network failure has no permanent verdict)", snap.State)
	}
}

func TestAuthCircuitOpen_BackoffGrowsAndCaps(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	authHealth.setState(AuthUnavailable, "first")
	_, retry1 := AuthCircuitOpen()
	authHealth.setState(AuthUnavailable, "second")
	_, retry2 := AuthCircuitOpen()

	if !retry2.After(retry1) {
		t.Fatalf("backoff did not grow: retry1=%v retry2=%v", retry1, retry2)
	}

	// Drive well past the cap and confirm it stays bounded.
	for range 10 {
		authHealth.setState(AuthUnavailable, "more")
	}
	_, retryCapped := AuthCircuitOpen()
	if d := time.Until(retryCapped); d > authCircuitMaxBackoff+time.Second {
		t.Fatalf("backoff exceeded cap: %v", d)
	}
}

func TestAuthCircuitOpen_RateLimitedDoesNotTripCircuit(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	authHealth.setState(AuthRateLimited, "app api rate limited")
	if open, _ := AuthCircuitOpen(); open {
		t.Fatal("rate_limited must not trip the misconfigured/unavailable circuit")
	}
}

func TestOnAuthRecovered_FiresOnceOnTransitionToHealthy(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	var fired atomic.Int32
	OnAuthRecovered(func() { fired.Add(1) })

	authHealth.setState(AuthUnavailable, "down")
	if fired.Load() != 0 {
		t.Fatal("recovery callback fired before any recovery")
	}
	authHealth.setState(AuthRefreshing, "retrying")
	if fired.Load() != 0 {
		t.Fatal("recovery callback fired on the in-between refreshing state")
	}
	authHealth.setState(AuthHealthy, "")
	if fired.Load() != 1 {
		t.Fatalf("fired = %d, want 1 after recovery", fired.Load())
	}

	// A second healthy->healthy transition must not re-fire.
	authHealth.setState(AuthHealthy, "")
	if fired.Load() != 1 {
		t.Fatalf("fired = %d, want still 1 (no spurious re-fire)", fired.Load())
	}
}

func TestRecordSuppressedCall_IncrementsSnapshot(t *testing.T) {
	t.Cleanup(resetAuthHealthForTest)
	resetAuthHealthForTest()

	RecordSuppressedCall()
	RecordSuppressedCall()

	if got := AuthHealthSnapshot().SuppressedCalls; got != 2 {
		t.Fatalf("SuppressedCalls = %d, want 2", got)
	}
}

func TestForceRefreshAppToken_ConcurrentCallsCollapseToOneMint(t *testing.T) {
	t.Cleanup(DisableAppAuth)
	path, _ := writeTestKey(t, false)

	var mints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Hold the handler open briefly so concurrent callers are guaranteed
		// to observe refreshMu.refreshing != nil rather than racing to
		// finish before the next goroutine even starts.
		time.Sleep(50 * time.Millisecond)
		n := mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_concurrent_%d","expires_at":"%s"}`,
			n, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer srv.Close()
	origBase := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = origBase })

	if err := EnableAppAuth(AppCredentials{AppID: 1, InstallationID: 2, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			errs[i] = ForceRefreshAppToken(t.Context())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	if got := mints.Load(); got != 1 {
		t.Fatalf("mints = %d, want exactly 1 for %d concurrent force-refresh callers", got, n)
	}
}

func TestClassifyMintError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want AuthState
	}{
		{"nil-typed error defaults unavailable", fmt.Errorf("boom"), AuthUnavailable},
		{"401 is misconfigured", &mintError{statusCode: http.StatusUnauthorized, cause: fmt.Errorf("x")}, AuthMisconfigured},
		{"404 is misconfigured", &mintError{statusCode: http.StatusNotFound, cause: fmt.Errorf("x")}, AuthMisconfigured},
		{"422 is misconfigured", &mintError{statusCode: http.StatusUnprocessableEntity, cause: fmt.Errorf("x")}, AuthMisconfigured},
		{"403 generic is misconfigured", &mintError{statusCode: http.StatusForbidden, message: "Resource not accessible", cause: fmt.Errorf("x")}, AuthMisconfigured},
		{"403 rate limit is rate_limited", &mintError{statusCode: http.StatusForbidden, message: "API rate limit exceeded", cause: fmt.Errorf("x")}, AuthRateLimited},
		{"500 is unavailable", &mintError{statusCode: http.StatusInternalServerError, cause: fmt.Errorf("x")}, AuthUnavailable},
		{"network failure (no status) is unavailable", &mintError{cause: fmt.Errorf("dial tcp: connection refused")}, AuthUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyMintError(tt.err); got != tt.want {
				t.Errorf("classifyMintError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestAuthCircuitOpenError_ClassifiedAsAuthError(t *testing.T) {
	err := NewAuthCircuitOpenError(time.Now().Add(time.Minute))
	if !IsAuthError(err) {
		t.Fatal("a circuit-open error must classify as an auth error so callers back off instead of misreading it")
	}
	if !strings.Contains(err.Error(), "circuit open") {
		t.Fatalf("Error() = %q, want it to mention the circuit", err.Error())
	}
}
