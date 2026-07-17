package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// AppCredentials identifies a GitHub App installation. The private key stays on
// disk — only its path is held.
type AppCredentials struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
}

// appTokenSource mints and caches GitHub App installation tokens. Installation
// tokens last ~1h; we refresh a few minutes early. A minted token is injected
// into the gh subprocess via GH_TOKEN, lifting the REST ceiling to 15k/hr
// without rewriting any call site.
type appTokenSource struct {
	creds  AppCredentials
	key    *rsa.PrivateKey
	client *http.Client

	mu      sync.RWMutex
	token   string
	expires time.Time
	slug    string
}

const appTokenRenewBefore = 5 * time.Minute

// appAPIBaseURL is the GitHub REST host for minting installation tokens.
// Overridable in tests.
var appAPIBaseURL = "https://api.github.com"

// appSource is the package-global token source. nil = App auth disabled, in
// which case gh uses its own auth and nothing is injected.
var (
	appSourceMu sync.RWMutex
	appSource   *appTokenSource
)

// EnableAppAuth configures GitHub App installation-token auth. It loads and
// validates the private key so a misconfiguration fails loudly at startup
// rather than silently on the first gh call. Pass a zero-value creds (or call
// DisableAppAuth) to turn it off.
func EnableAppAuth(creds AppCredentials) error {
	if creds.AppID == 0 || creds.InstallationID == 0 || creds.PrivateKeyPath == "" {
		return fmt.Errorf("github app auth: app_id, installation_id and private_key_path are required")
	}
	key, err := loadPrivateKey(creds.PrivateKeyPath)
	if err != nil {
		return err
	}
	src := &appTokenSource{
		creds:  creds,
		key:    key,
		client: &http.Client{Timeout: 15 * time.Second},
	}
	appSourceMu.Lock()
	appSource = src
	appSourceMu.Unlock()
	// The viewer identity is auth-mode-dependent (<slug>[bot] under App auth,
	// the /user login otherwise), so a mode switch must not inherit the
	// previous mode's cached login.
	resetCachedViewer()
	return nil
}

// DisableAppAuth clears any configured App auth (used by tests and config
// reload when the App block is removed).
func DisableAppAuth() {
	appSourceMu.Lock()
	appSource = nil
	appSourceMu.Unlock()
	resetCachedViewer()
}

func currentAppSource() *appTokenSource {
	appSourceMu.RLock()
	defer appSourceMu.RUnlock()
	return appSource
}

// RefreshAppToken mints or renews the installation token if App auth is enabled
// and the cached token is missing or near expiry. Safe to call on a timer and
// at startup; a no-op when App auth is disabled.
func RefreshAppToken(ctx context.Context) error {
	src := currentAppSource()
	if src == nil {
		return nil
	}
	return src.refresh(ctx)
}

// cachedAppToken returns the current installation token, or "" when App auth is
// disabled or no token has been minted yet. Non-blocking — never performs I/O —
// so the request gate is never stalled on a token mint.
func CurrentAppToken() string {
	return cachedAppToken()
}

func cachedAppToken() string {
	src := currentAppSource()
	if src == nil {
		return ""
	}
	src.mu.RLock()
	defer src.mu.RUnlock()
	if src.token == "" || time.Now().After(src.expires) {
		return ""
	}
	return src.token
}

func (s *appTokenSource) refresh(ctx context.Context) error {
	s.mu.RLock()
	fresh := s.token != "" && time.Until(s.expires) > appTokenRenewBefore
	s.mu.RUnlock()
	if fresh {
		return nil
	}

	jwt, err := s.signJWT(time.Now())
	if err != nil {
		return err
	}
	token, expires, err := s.mintInstallationToken(ctx, jwt)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.token, s.expires = token, expires
	s.mu.Unlock()
	return nil
}

// signJWT builds the short-lived App JWT (RS256) used to authenticate the
// installation-token request. Hand-rolled to avoid a JWT dependency.
func (s *appTokenSource) signJWT(now time.Time) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sign app jwt: app auth is disabled")
	}
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// iat backdated 60s to tolerate clock skew; exp capped at 10m (GitHub max).
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%d"}`,
		now.Add(-60*time.Second).Unix(), now.Add(9*time.Minute).Unix(), s.creds.AppID)
	signingInput := header + "." + base64URL([]byte(claims))
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *appTokenSource) mintInstallationToken(ctx context.Context, jwt string) (string, time.Time, error) {
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", appAPIBaseURL, s.creds.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", time.Time{}, fmt.Errorf("decode installation token: %w", err)
	}
	if resp.StatusCode != http.StatusCreated || body.Token == "" {
		return "", time.Time{}, fmt.Errorf("mint installation token: HTTP %d: %s", resp.StatusCode, body.Message)
	}
	expires, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		// Fall back to a conservative 50m lifetime if GitHub omits the field.
		expires = time.Now().Add(50 * time.Minute)
	}
	return body.Token, expires, nil
}

// appLogin returns the App's bot login as it appears on GitHub artifacts —
// "<slug>[bot]", e.g. "sybra-app[bot]". This is the App-auth answer to "who am
// I?": /user (see ViewerLogin) is a user-to-server endpoint and always 403s for
// installation tokens, so it can never identify an App. GET /app is JWT-authed
// and returns the slug, which is immutable for the lifetime of the App and so
// is cached indefinitely.
func (s *appTokenSource) appLogin(ctx context.Context) (string, error) {
	if s == nil {
		return "", fmt.Errorf("app login: app auth is disabled")
	}
	s.mu.RLock()
	slug := s.slug
	s.mu.RUnlock()
	if slug != "" {
		return slug + "[bot]", nil
	}

	jwt, err := s.signJWT(time.Now())
	if err != nil {
		return "", err
	}
	slug, err = s.fetchAppSlug(ctx, jwt)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.slug = slug
	s.mu.Unlock()
	return slug + "[bot]", nil
}

func (s *appTokenSource) fetchAppSlug(ctx context.Context, jwt string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, appAPIBaseURL+"/app", http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch app slug: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body struct {
		Slug    string `json:"slug"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode app slug: %w", err)
	}
	if resp.StatusCode != http.StatusOK || body.Slug == "" {
		return "", fmt.Errorf("fetch app slug: HTTP %d: %s", resp.StatusCode, body.Message)
	}
	return body.Slug, nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read app private key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("app private key: no PEM block in %s", path)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app private key: parse %s: %w", path, err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("app private key: %s is not an RSA key", path)
	}
	return rsaKey, nil
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ghEnv returns the environment for a gh subprocess, injecting GH_TOKEN when an
// App installation token is available. Returns nil to mean "inherit unchanged"
// so non-App setups keep gh's own auth.
func ghEnv() []string {
	token := cachedAppToken()
	if token == "" {
		return nil
	}
	return append(os.Environ(), "GH_TOKEN="+token)
}

// GHEnv is the exported form of ghEnv for gh subprocesses spawned outside
// this package. internal/monitor's GHIssueSink shells out to gh directly
// (it predates the App-auth mechanism) and must inject the same
// installation token as every gh call in this package, otherwise it falls
// back to ambient `gh auth login`/GH_TOKEN even when a GitHub App is
// configured and healthy — see issue #2032.
func GHEnv() []string {
	return ghEnv()
}

// Authenticated reports whether gh can currently reach the API under the
// configured credentials — a cached GitHub App installation token, an
// ambient GH_TOKEN carrying one, or ambient user gh auth. Performs a live
// lookup, so it's meant for a startup/periodic preflight, not a hot path.
//
// Deliberately does NOT use ViewerLogin()/gh api user: /user is a
// user-to-server endpoint that always 403s for GitHub App installation
// tokens even when they're fully functional for issue filing, which made
// the preflight false-positive on exactly the credential type it exists to
// support (see #2032). /rate_limit is reachable by every credential type gh
// supports, so probe that instead.
func Authenticated() bool {
	return authenticated(defaultExecer)
}

func authenticated(e execer) bool {
	_, err := e.run("api", "rate_limit", "-q", ".rate.limit")
	return err == nil
}
