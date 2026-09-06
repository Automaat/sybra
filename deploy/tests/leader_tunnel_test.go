//go:build !windows

package deploy_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLeaderTunnelTracksAuthenticatedHomeAndPort(t *testing.T) {
	for _, executable := range []string{"bash", "curl", "jq", "shasum"} {
		if _, err := exec.LookPath(executable); err != nil {
			t.Fatalf("tunnel prerequisite %s unavailable: %v", executable, err)
		}
	}
	root := t.TempDir()
	home, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(home)))
	var healthy atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		homeID := "wrong-board"
		if healthy.Load() {
			homeID = identity
		}
		_, _ = fmt.Fprintf(w, `{"status":"ok","service":"sybra","home_id":%q}`, homeID)
	})
	first := httptest.NewServer(handler)
	defer first.Close()
	second := httptest.NewServer(handler)
	defer second.Close()
	port := func(server *httptest.Server) string { return strings.TrimPrefix(server.URL, "http://127.0.0.1:") }
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "desktop-port"), port(first), 0o600)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "ssh-trace")
	// Only SSH and the reconciliation delay are replaced. Health checks use
	// real curl against real HTTP servers, including a wrong-board HTTP 200.
	write(filepath.Join(bin, "ssh"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYBRA_TUNNEL_TRACE\"\nexec /bin/sleep 60\n", 0o700)
	write(filepath.Join(bin, "sleep"), "#!/bin/sh\nexec /bin/sleep 0.02\n", 0o700)
	cmd := exec.Command("bash", "../bin/sybra-leader-tunnel.sh", root, "test-worker", "18081")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SYBRA_TUNNEL_TRACE="+trace)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("tunnel did not stop promptly")
		}
	})
	readTrace := func() string {
		data, _ := os.ReadFile(trace)
		return string(data)
	}
	time.Sleep(200 * time.Millisecond)
	if readTrace() != "" {
		t.Fatal("wrong-board health response opened a credential-bearing tunnel")
	}
	await := func(description string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for !condition() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !condition() {
			t.Fatalf("%s: trace=%q", description, readTrace())
		}
	}
	healthy.Store(true)
	await("initial forward", func() bool { return strings.Contains(readTrace(), "127.0.0.1:18081:127.0.0.1:"+port(first)) })
	write(filepath.Join(root, "desktop-port"), port(second)+"\n", 0o600)
	await("moved leader forward", func() bool { return strings.Contains(readTrace(), "127.0.0.1:18081:127.0.0.1:"+port(second)) })
	write(filepath.Join(root, "desktop-port"), "not-a-port\n", 0o600)
	time.Sleep(150 * time.Millisecond)
	before := readTrace()
	time.Sleep(150 * time.Millisecond)
	if readTrace() != before {
		t.Fatal("invalid listener port spawned SSH")
	}
}
