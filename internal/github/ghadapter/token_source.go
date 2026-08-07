// Package ghadapter is an evaluation spike for issue #3209: does a
// maintained GitHub App auth / webhook library remove commodity protocol
// code from internal/github/appauth.go and cmd/sybra-server/webhook_github.go
// without giving up the safety behavior those hand-rolled implementations
// carry (nonblocking cached-token reads, collapsed concurrent refreshes,
// forced-refresh-on-401 recovery)?
//
// Nothing in this package is wired into any dispatch path. See
// docs/github-app-webhook-adapter-spike.md for the findings and the
// adopt/retain decision.
package ghadapter

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// ghAPIBaseURL is the GitHub REST host. Overridable in tests, mirroring
// internal/github/appauth.go's appAPIBaseURL.
var ghAPIBaseURL = "https://api.github.com"

// TokenSource wraps a ghinstallation.Transport to stand in for
// internal/github's hand-rolled appTokenSource. It exists because
// ghinstallation.Transport.Token(ctx) alone does not preserve either of the
// two safety contracts appTokenSource guarantees:
//
//   - GHEnv() must never perform I/O: it reads a cached token or returns "",
//     it never mints one. Transport.Token mints synchronously under its
//     internal lock whenever the cached token is expired — calling it
//     directly from GHEnv() would stall the gh subprocess request gate on a
//     network round trip. Cached() reproduces the nonblocking read from a
//     snapshot this type keeps itself; it never touches the Transport at all
//     (see the mintMu/cacheMu split below for why gating on
//     Transport.Expiry() instead does not hold the invariant).
//   - ForceRefreshAppToken (#2453) always mints, and collapses N concurrent
//     callers into one HTTP call. ghinstallation.Transport has no
//     "invalidate" or "force" primitive — the only refresh trigger is
//     isExpired(). Forcing a remint here means discarding the Transport and
//     building a new one (cheap: RSA key parsing, no I/O), and ForceRefresh
//     re-implements the same singleflight-by-channel pattern
//     appTokenSource.refresh already uses, because swapping the Transport
//     buys none of that collapsing for free.
type TokenSource struct {
	appID          int64
	installationID int64
	keyPEM         []byte

	// mintMu serializes every access to cur, including ghinstallation's own
	// Token() and Expiry(). Expiry() reads Transport's cached token without
	// taking the library's internal mutex, which Token() does hold while
	// writing it — so an Expiry() call that isn't serialized against minting
	// is a plain data race. mintMu is held across the network mint.
	mintMu sync.Mutex
	cur    *ghinstallation.Transport

	// cacheMu guards the token snapshot Cached() reads, recorded after each
	// successful mint. It is deliberately separate from mintMu and held only
	// for the copy in and out, never across I/O: that is what keeps Cached()
	// nonblocking while a refresh is in flight. Gating Cached() on
	// Transport.Expiry() instead cannot give that — the token can cross
	// refreshAt between the check and the following Token(), which then mints
	// synchronously on the supposedly nonblocking path.
	cacheMu   sync.RWMutex
	token     string
	refreshAt time.Time

	forceMu  sync.Mutex
	forcing  chan struct{}
	forceErr error

	slugMu sync.Mutex
	slug   string
}

// NewTokenSource builds a TokenSource for the given App installation. The
// private key must be a PEM-encoded RSA key (PKCS1 or PKCS8). Construction
// parses the key but performs no I/O and mints no token — mirroring
// EnableAppAuth's "fail loudly at startup on bad config, mint lazily"
// contract.
func NewTokenSource(appID, installationID int64, privateKeyPEM []byte) (*TokenSource, error) {
	if appID <= 0 || installationID <= 0 {
		return nil, fmt.Errorf("ghadapter: app id and installation id must be positive")
	}
	transport, err := newTransport(appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("ghadapter: build installation transport: %w", err)
	}
	return &TokenSource{
		appID:          appID,
		installationID: installationID,
		keyPEM:         privateKeyPEM,
		cur:            transport,
	}, nil
}

func newTransport(appID, installationID int64, privateKeyPEM []byte) (*ghinstallation.Transport, error) {
	t, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKeyPEM)
	if err != nil {
		return nil, err
	}
	t.BaseURL = ghAPIBaseURL
	return t, nil
}

// Cached returns the last minted installation token without performing any
// I/O and without touching the ghinstallation.Transport. It returns "" when
// no token has been minted yet, or when the recorded one has reached the
// library's own refresh window (Expiry()'s refreshAt) — in both cases a
// caller must go through Refresh/ForceRefresh instead, the same contract
// cachedAppToken() gives GHEnv().
func (s *TokenSource) Cached() string {
	s.cacheMu.RLock()
	tok, refreshAt := s.token, s.refreshAt
	s.cacheMu.RUnlock()

	if tok == "" || !time.Now().Before(refreshAt) {
		return ""
	}
	return tok
}

func (s *TokenSource) storeCached(token string, refreshAt time.Time) {
	s.cacheMu.Lock()
	s.token, s.refreshAt = token, refreshAt
	s.cacheMu.Unlock()
}

// mintFrom returns t's token plus the instant Transport itself starts
// treating it as stale. Both library calls must be serialized against every
// other use of t by the caller — see TokenSource.mintMu.
func mintFrom(ctx context.Context, t *ghinstallation.Transport) (string, time.Time, error) {
	tok, err := t.Token(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	_, refreshAt, err := t.Expiry()
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, refreshAt, nil
}

// Refresh mints a token if the cached one is missing or within the
// library's refresh window; a no-op otherwise. Safe to call on a timer —
// mirrors RefreshAppToken.
func (s *TokenSource) Refresh(ctx context.Context) error {
	s.mintMu.Lock()
	defer s.mintMu.Unlock()

	tok, refreshAt, err := mintFrom(ctx, s.cur)
	if err != nil {
		// Leave the snapshot alone: a previously minted token stays valid
		// until its own expiry, so a failed renewal must not blank it.
		return err
	}
	s.storeCached(tok, refreshAt)
	return nil
}

// ForceRefresh always mints a new installation token, even if the cached one
// isn't near expiry by Transport's own bookkeeping — needed when a preflight
// observes a 401 right at the hourly rotation boundary that Refresh would
// otherwise no-op against (see appauth.go's ForceRefreshAppToken, #2453).
// Concurrent callers collapse into a single mint, same as
// forceRefreshAppTokenLeader.
func (s *TokenSource) ForceRefresh(ctx context.Context) error {
	s.forceMu.Lock()
	if ch := s.forcing; ch != nil {
		s.forceMu.Unlock()
		<-ch
		s.forceMu.Lock()
		err := s.forceErr
		s.forceMu.Unlock()
		return err
	}
	ch := make(chan struct{})
	s.forcing = ch
	s.forceMu.Unlock()

	var err error
	defer func() {
		s.forceMu.Lock()
		s.forceErr = err
		s.forcing = nil
		s.forceMu.Unlock()
		close(ch)
	}()

	fresh, mkErr := newTransport(s.appID, s.installationID, s.keyPEM)
	if mkErr != nil {
		err = mkErr
		return err
	}
	// fresh is not published yet, so this goroutine owns it exclusively and
	// mintFrom needs no lock here; mintMu is only taken to swap and record.
	tok, refreshAt, tokErr := mintFrom(ctx, fresh)
	if tokErr != nil {
		err = tokErr
		return err
	}

	s.mintMu.Lock()
	s.cur = fresh
	s.storeCached(tok, refreshAt)
	s.mintMu.Unlock()
	return nil
}

// AppLogin returns the App's bot login ("<slug>[bot]"), fetched once via
// GET /app (App-JWT-authed, not installation-token-authed) and cached for
// the process lifetime — the slug is immutable for the App's lifetime.
// Mirrors appTokenSource.appLogin.
func (s *TokenSource) AppLogin(ctx context.Context) (string, error) {
	s.slugMu.Lock()
	slug := s.slug
	s.slugMu.Unlock()
	if slug != "" {
		return slug + "[bot]", nil
	}

	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, s.appID, s.keyPEM)
	if err != nil {
		return "", fmt.Errorf("ghadapter: build app transport: %w", err)
	}
	atr.BaseURL = ghAPIBaseURL

	baseURL := ghAPIBaseURL + "/"
	client, err := github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: atr}),
		github.WithURLs(&baseURL, &baseURL),
	)
	if err != nil {
		return "", fmt.Errorf("ghadapter: build app client: %w", err)
	}

	app, _, err := client.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("ghadapter: fetch app slug: %w", err)
	}
	if app.GetSlug() == "" {
		return "", fmt.Errorf("ghadapter: fetch app slug: empty slug in response")
	}

	s.slugMu.Lock()
	s.slug = app.GetSlug()
	s.slugMu.Unlock()
	return app.GetSlug() + "[bot]", nil
}
