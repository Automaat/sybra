package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const certValidity = 10 * 365 * 24 * time.Hour

// GeneratedCert reports where a follower's self-signed keypair was written and
// the SHA-256 fingerprint the leader must pin. The fingerprint is computed from
// the certificate that was actually written, not from the inputs, so the printed
// pin can never drift from the file on disk.
type GeneratedCert struct {
	CertFile string    `json:"certFile"`
	KeyFile  string    `json:"keyFile"`
	Pin      string    `json:"pin"`
	Hosts    []string  `json:"hosts"`
	NotAfter time.Time `json:"notAfter"`
}

// GenerateFollowerCert writes a self-signed P-256 certificate and key for a
// follower's control plane and returns the leader's pin for it. There is no CA
// in this tier by design: the leader trusts exactly one fingerprint, so a
// self-signed leaf is the whole trust anchor.
func GenerateFollowerCert(dir string, hosts []string, now time.Time) (GeneratedCert, error) {
	if len(hosts) == 0 {
		return GeneratedCert{}, fmt.Errorf("at least one --host is required (the address the leader dials)")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return GeneratedCert{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return GeneratedCert{}, fmt.Errorf("generate serial: %w", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"sybra follower"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			continue
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return GeneratedCert{}, fmt.Errorf("create certificate: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return GeneratedCert{}, fmt.Errorf("create dir: %w", err)
	}

	certFile := filepath.Join(dir, "follower.crt")
	keyFile := filepath.Join(dir, "follower.key")
	if err := writePEM(certFile, "CERTIFICATE", der, 0o644); err != nil {
		return GeneratedCert{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return GeneratedCert{}, fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyFile, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return GeneratedCert{}, err
	}

	sum := sha256.Sum256(der)
	return GeneratedCert{
		CertFile: certFile,
		KeyFile:  keyFile,
		Pin:      hex.EncodeToString(sum[:]),
		Hosts:    hosts,
		NotAfter: tmpl.NotAfter,
	}, nil
}

func writePEM(path, blockType string, der []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		_ = f.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}
