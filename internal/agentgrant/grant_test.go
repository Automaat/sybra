package agentgrant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func store(t *testing.T, ttl time.Duration) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "grants.json"), ttl)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestMint_IsScopedAndVerifiable pins the credential's basic contract: it
// resolves to the run it was minted for and nothing else does.
func TestMint_IsScopedAndVerifiable(t *testing.T) {
	s := store(t, time.Hour)
	token, err := s.Mint("task-a")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	grant, ok := s.Verify(token)
	if !ok || grant.TaskID != "task-a" {
		t.Fatalf("Verify = %+v ok=%v, want task-a", grant, ok)
	}
	if _, ok := s.Verify("not-a-grant"); ok {
		t.Fatal("an unknown credential verified")
	}
	if _, ok := s.Verify(""); ok {
		t.Fatal("an empty credential verified")
	}
}

// TestMint_StoresOnlyADigest is the reason this exists: the board's own token
// was kept in the clear, so anything that could read the store held the board.
func TestMint_StoresOnlyADigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	s, err := New(path, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, err := s.Mint("task-a")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	data, err := readFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if contains(data, token) {
		t.Fatal("the credential itself is on disk; reading the store hands over a usable one")
	}
	if !contains(data, "task-a") {
		t.Error("the store does not record which run a grant belongs to")
	}
}

// TestRevoke_EndsTheCredentialWithTheRun pins what the board token could never
// do: end one agent's access without rotating everything else.
func TestRevoke_EndsTheCredentialWithTheRun(t *testing.T) {
	s := store(t, time.Hour)
	first, err := s.Mint("task-a")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	second, err := s.Mint("task-b")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if err := s.Revoke("task-a"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := s.Verify(first); ok {
		t.Fatal("a revoked credential still verifies")
	}
	if _, ok := s.Verify(second); !ok {
		t.Fatal("revoking one run's credential took another run's with it")
	}
}

// TestVerify_RefusesALapsedGrant pins the expiry a killed run leaves behind.
func TestVerify_RefusesALapsedGrant(t *testing.T) {
	s := store(t, time.Nanosecond)
	token, err := s.Mint("task-a")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := s.Verify(token); ok {
		t.Fatal("a lapsed credential still verifies")
	}
}

// TestNew_ReloadsAcrossRestarts keeps a live run's credential working when the
// board restarts under it.
func TestNew_ReloadsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.json")
	first, err := New(path, time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	token, err := first.Mint("task-a")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	restarted, err := New(path, time.Hour)
	if err != nil {
		t.Fatalf("New(restart): %v", err)
	}
	if _, ok := restarted.Verify(token); !ok {
		t.Fatal("a live run's credential stopped working when the board restarted")
	}
}
