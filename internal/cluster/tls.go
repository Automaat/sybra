package cluster

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ParsePin normalizes a cert-pin fingerprint: a hex SHA-256 (64 hex chars,
// with optional colons or "sha256:" prefix) of a follower's leaf certificate.
func ParsePin(pin string) ([]byte, error) {
	clean := strings.ToLower(strings.TrimSpace(pin))
	clean = strings.TrimPrefix(clean, "sha256:")
	clean = strings.ReplaceAll(clean, ":", "")
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("cluster: invalid tls_pin %q: %w", pin, err)
	}
	if len(raw) != sha256.Size {
		return nil, fmt.Errorf("cluster: tls_pin must be a %d-byte SHA-256 (%d hex chars), got %d bytes", sha256.Size, sha256.Size*2, len(raw))
	}
	return raw, nil
}

func pinnedTransport(pin string) (*http.Transport, error) {
	want, err := ParsePin(pin)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("cluster: follower presented no certificate")
				}
				got := sha256.Sum256(cs.PeerCertificates[0].Raw)
				if subtle.ConstantTimeCompare(got[:], want) != 1 {
					return fmt.Errorf("cluster: cert pin mismatch (got sha256:%x)", got)
				}
				return nil
			},
		},
	}
	return tr, nil
}
