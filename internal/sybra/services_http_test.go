package sybra

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/artifact"
	"github.com/Automaat/sybra/internal/httpapi"
	"github.com/Automaat/sybra/internal/learning"
)

// TestLearningServiceHTTPAllowlist verifies the confidentiality contract for
// the raw learning store: ListDigests and GetLatestDigest are reachable over
// HTTP, but StoreDigest (which writes unscrubbed digests) is not.
func TestLearningServiceHTTPAllowlist(t *testing.T) {
	store, err := learning.New(t.TempDir())
	if err != nil {
		t.Fatalf("learning.New: %v", err)
	}
	a := &App{learningSvc: &LearningService{store: store}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	post := func(method string) int {
		resp, err := http.Post(srv.URL+"/api/LearningService/"+method, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", method, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if status := post("ListDigests"); status == http.StatusNotFound {
		t.Errorf("ListDigests should be reachable over HTTP, got %d", status)
	}
	if status := post("GetLatestDigest"); status == http.StatusNotFound {
		t.Errorf("GetLatestDigest should be reachable over HTTP, got %d", status)
	}
	if status := post("StoreDigest"); status != http.StatusNotFound {
		t.Errorf("StoreDigest must NOT be reachable over HTTP, got %d (want 404)", status)
	}
}

// TestTaskServiceHTTPAllowlist_ListTaskProgress guards against the Progress
// tab regressing to unreachable-over-HTTP: the method exists on TaskService
// and works in-process (Wails desktop), but was never added to the
// coreHTTPServices allowlist, so the server/web surface 404'd on it.
func TestTaskServiceHTTPAllowlist_ListTaskProgress(t *testing.T) {
	a := &App{taskSvc: &TaskService{}}

	mux := http.NewServeMux()
	httpapi.Mount(mux, ServiceRegistry(a), slog.Default())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/TaskService/ListTaskProgress", "application/json", strings.NewReader(`["any"]`))
	if err != nil {
		t.Fatalf("POST ListTaskProgress: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ListTaskProgress status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var entries []artifact.ProgressEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatalf("decode ListTaskProgress response: %v", err)
	}
	if entries == nil {
		t.Fatal("ListTaskProgress response decoded to nil, want empty slice")
	}
}
