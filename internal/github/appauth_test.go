package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTestKey(t *testing.T, pkcs8 bool) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	var der []byte
	var typ string
	if pkcs8 {
		der, err = x509.MarshalPKCS8PrivateKey(key)
		typ = "PRIVATE KEY"
	} else {
		der = x509.MarshalPKCS1PrivateKey(key)
		typ = "RSA PRIVATE KEY"
	}
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, key
}

func TestLoadPrivateKey_PKCS1AndPKCS8(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		path, _ := writeTestKey(t, pkcs8)
		if _, err := loadPrivateKey(path); err != nil {
			t.Fatalf("pkcs8=%v load: %v", pkcs8, err)
		}
	}
}

func TestSignJWT_VerifiableRS256(t *testing.T) {
	path, key := writeTestKey(t, false)
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	src := currentAppSource()
	now := time.Unix(1_700_000_000, 0)
	tok, err := src.signJWT(now)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts, want 3", len(parts))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(claims, &c); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if c.Iss != "42" {
		t.Fatalf("iss = %q, want 42", c.Iss)
	}
}

func TestRefreshAppToken_MintsAndInjectsEnv(t *testing.T) {
	path, _ := writeTestKey(t, false)
	t.Cleanup(DisableAppAuth)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing bearer jwt")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_installationtoken","expires_at":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	}))
	defer srv.Close()
	origBase := appAPIBaseURL
	appAPIBaseURL = srv.URL
	t.Cleanup(func() { appAPIBaseURL = origBase })

	if err := EnableAppAuth(AppCredentials{AppID: 42, InstallationID: 7, PrivateKeyPath: path}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if cachedAppToken() != "" {
		t.Fatal("expected no token before refresh")
	}
	if err := RefreshAppToken(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := cachedAppToken(); got != "ghs_installationtoken" {
		t.Fatalf("cached token = %q", got)
	}
	env := ghEnv()
	var found bool
	for _, kv := range env {
		if kv == "GH_TOKEN=ghs_installationtoken" {
			found = true
		}
	}
	if !found {
		t.Fatalf("GH_TOKEN not injected into gh env")
	}

	// Disabled source injects nothing.
	DisableAppAuth()
	if ghEnv() != nil {
		t.Fatal("expected nil env when app auth disabled")
	}
}

func TestEnableAppAuth_RequiresAllFields(t *testing.T) {
	t.Cleanup(DisableAppAuth)
	if err := EnableAppAuth(AppCredentials{AppID: 1}); err == nil {
		t.Fatal("expected error for incomplete credentials")
	}
}
