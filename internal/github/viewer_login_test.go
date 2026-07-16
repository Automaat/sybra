package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// emptyLoginExecer answers the reviews call with valid JSON but yields an empty
// /user login, isolating identity failure from reviews-fetch failure.
type emptyLoginExecer struct{}

func (e *emptyLoginExecer) run(args ...string) ([]byte, error) {
	if slices.Contains(args, "user") {
		return []byte(""), nil
	}
	return []byte(`[]`), nil
}

// clearViewerCache isolates a test from the package-global viewer memo.
func clearViewerCache(t *testing.T) {
	t.Helper()
	viewerMu.Lock()
	prev := cachedViewer
	cachedViewer = ""
	viewerMu.Unlock()
	t.Cleanup(func() {
		viewerMu.Lock()
		cachedViewer = prev
		viewerMu.Unlock()
	})
}

// appSlugServer stands in for GitHub's JWT-authed GET /app.
func appSlugServer(t *testing.T, slug string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			t.Errorf("unexpected path %q, want /app", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("GET /app must be JWT-authed")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"` + slug + `"}`))
	}))
	t.Cleanup(srv.Close)
	orig := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = orig })
}

// Under App auth the identity must come from GET /app, never /user: /user is a
// user-to-server endpoint that always 403s for installation tokens, which is
// the root cause of the #2164 review loop.
func TestViewerLogin_AppAuthResolvesSlugAndNeverCallsUser(t *testing.T) {
	clearViewerCache(t)
	path, _ := writeTestKey(t, false)
	appSlugServer(t, "sybra-app")
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// A 403 from /user, exactly as GitHub answers an installation token.
	fe := &recordingExecer{err: errors.New("gh: HTTP 403: Resource not accessible by integration")}

	got, err := viewerLoginE(context.Background(), fe)
	if err != nil {
		t.Fatalf("viewerLoginE: %v", err)
	}
	if got != "sybra-app[bot]" {
		t.Errorf("login = %q, want sybra-app[bot]", got)
	}
	if fe.calls != 0 {
		t.Errorf("gh was called %d times; /user must not be reached under App auth", fe.calls)
	}
}

// The App slug is immutable, so it is fetched once and memoized.
func TestViewerLogin_AppAuthCachesSlug(t *testing.T) {
	clearViewerCache(t)
	path, _ := writeTestKey(t, false)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slug":"sybra-app"}`))
	}))
	defer srv.Close()
	orig := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = orig })
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	for range 3 {
		if _, err := viewerLoginE(context.Background(), &recordingExecer{}); err != nil {
			t.Fatalf("viewerLoginE: %v", err)
		}
	}
	if hits != 1 {
		t.Errorf("GET /app called %d times, want 1 (slug is immutable)", hits)
	}
}

// Without App auth the /user path is correct and must be preserved.
func TestViewerLogin_UserAuthUsesUserEndpoint(t *testing.T) {
	clearViewerCache(t)
	DisableAppAuth()

	fe := &recordingExecer{output: []byte("octocat\n")}
	got, err := viewerLoginE(context.Background(), fe)
	if err != nil {
		t.Fatalf("viewerLoginE: %v", err)
	}
	if got != "octocat" {
		t.Errorf("login = %q, want octocat", got)
	}
	if !slices.Contains(fe.lastArgs, "user") {
		t.Errorf("user auth must resolve identity via /user; got args %v", fe.lastArgs)
	}
}

// The bare "" return swallowed the reason for 23 hours during #2164. The cause
// must reach the caller.
func TestViewerLoginE_SurfacesCause(t *testing.T) {
	clearViewerCache(t)
	DisableAppAuth()

	fe := &recordingExecer{err: errors.New("HTTP 403: Resource not accessible by integration")}
	_, err := viewerLoginE(context.Background(), fe)
	if err == nil {
		t.Fatal("expected an error when identity cannot be resolved")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q must carry the underlying cause", err)
	}

	// The legacy string-only wrapper still degrades to "" for its callers.
	if got := viewerLogin(context.Background(), fe); got != "" {
		t.Errorf("viewerLogin = %q, want empty on failure", got)
	}
}

// An empty login is a failure, not a valid identity — attributing reviews to ""
// would silently match nothing.
func TestViewerLoginE_EmptyLoginIsAnError(t *testing.T) {
	clearViewerCache(t)
	DisableAppAuth()

	if _, err := viewerLoginE(context.Background(), &recordingExecer{output: []byte("  \n")}); err == nil {
		t.Fatal("expected an error for an empty /user login")
	}
}

// The regression test for #2164: under App auth, the bot's own review must be
// attributed to the viewer. This is what reconcileReviewPhases needs to see in
// order to leave needs-approval, and its failure drove 112 reviews on one PR.
func TestFetchMyReviewState_AttributesAppBotReview(t *testing.T) {
	clearViewerCache(t)
	path, _ := writeTestKey(t, false)
	appSlugServer(t, "sybra-app")
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	body := `[{"state":"CHANGES_REQUESTED","user":{"login":"sybra-app[bot]"},` +
		`"commit_id":"e57e4b5","submitted_at":"2026-07-15T18:48:08Z"}]`
	got, err := fetchMyReviewStateWith(&recordingExecer{output: []byte(body)}, "o/r", 151)
	if err != nil {
		t.Fatalf("fetchMyReviewState: %v", err)
	}
	want := MyReviewState{Submitted: true, Approved: false, ReviewedSHA: "e57e4b5"}
	if got != want {
		t.Errorf("got %+v, want %+v — the bot must recognise its own review", got, want)
	}
}

// FetchMyReviewState's error must name the cause, so a persistent auth failure
// is not mistaken for a transient blip.
func TestFetchMyReviewState_ErrorCarriesViewerCause(t *testing.T) {
	clearViewerCache(t)
	DisableAppAuth()

	_, err := fetchMyReviewStateWith(&emptyLoginExecer{}, "o/r", 151)
	if err == nil {
		t.Fatal("expected an error when the viewer cannot be resolved")
	}
	if !strings.Contains(err.Error(), "resolve viewer login") {
		t.Errorf("error %q should name the failing operation", err)
	}
}

// Switching auth mode must not inherit the previous mode's identity.
func TestAppAuthToggle_ResetsCachedViewer(t *testing.T) {
	clearViewerCache(t)
	DisableAppAuth()

	if _, err := viewerLoginE(context.Background(), &recordingExecer{output: []byte("octocat")}); err != nil {
		t.Fatalf("seed viewer: %v", err)
	}
	viewerMu.RLock()
	seeded := cachedViewer
	viewerMu.RUnlock()
	if seeded != "octocat" {
		t.Fatalf("cachedViewer = %q, want octocat", seeded)
	}

	path, _ := writeTestKey(t, false)
	appSlugServer(t, "sybra-app")
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got, err := viewerLoginE(context.Background(), &recordingExecer{output: []byte("octocat")})
	if err != nil {
		t.Fatalf("viewerLoginE: %v", err)
	}
	if got != "sybra-app[bot]" {
		t.Errorf("login = %q, want sybra-app[bot] — the user-auth identity must not survive the switch", got)
	}
}
