package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/cluster"
)

func TestGeneratedCertIsAcceptedByAPinnedLeader(t *testing.T) {
	dir := t.TempDir()
	got, err := GenerateFollowerCert(dir, []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("GenerateFollowerCert: %v", err)
	}

	keypair, err := tls.LoadX509KeyPair(got.CertFile, got.KeyFile)
	if err != nil {
		t.Fatalf("the generated keypair does not load: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/TaskService/ListTasks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	node := cluster.Node{Name: "pinned", Endpoints: []string{srv.URL}, TLSPin: got.Pin}
	client, err := cluster.NewClient(node, nil)
	if err != nil || client == nil {
		t.Fatalf("NewClient with the printed pin: %v", err)
	}
	if _, err := client.ListTasks(t.Context()); err != nil {
		t.Fatalf("a leader pinning the printed fingerprint must reach the follower: %v", err)
	}
}

func TestPinnedLeaderRejectsADifferentCert(t *testing.T) {
	genuine, err := GenerateFollowerCert(t.TempDir(), []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("GenerateFollowerCert: %v", err)
	}
	impostor, err := GenerateFollowerCert(t.TempDir(), []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("GenerateFollowerCert: %v", err)
	}
	if genuine.Pin == impostor.Pin {
		t.Fatal("two independently generated certs must not share a fingerprint")
	}

	keypair, err := tls.LoadX509KeyPair(impostor.CertFile, impostor.KeyFile)
	if err != nil {
		t.Fatalf("load impostor keypair: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.NewServeMux())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	node := cluster.Node{Name: "impostor", Endpoints: []string{srv.URL}, TLSPin: genuine.Pin}
	client, err := cluster.NewClient(node, nil)
	if err != nil || client == nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.ListTasks(t.Context()); err == nil {
		t.Fatal("a node serving a certificate the leader did not pin must be refused")
	}
}

func TestGenerateFollowerCertRequiresAHost(t *testing.T) {
	if _, err := GenerateFollowerCert(t.TempDir(), nil, time.Now()); err == nil {
		t.Fatal("a cert with no host is unusable for a leader that dials by address")
	}
}

func TestGeneratedKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	got, err := GenerateFollowerCert(dir, []string{"127.0.0.1"}, time.Now())
	if err != nil {
		t.Fatalf("GenerateFollowerCert: %v", err)
	}
	info, err := os.Stat(got.KeyFile)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", perm)
	}
	pem, err := os.ReadFile(got.CertFile)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	if !strings.Contains(string(pem), "BEGIN CERTIFICATE") {
		t.Fatalf("cert file is not PEM: %s", filepath.Base(got.CertFile))
	}
}
