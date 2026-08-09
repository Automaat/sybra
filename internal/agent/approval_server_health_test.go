package agent

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Automaat/sybra/internal/httpserve"
)

// TestApprovalServerHealth_AnswersAsTheVerifierControlChannel pins the body a
// verifier's own CLI probes before it will send its task-scoped token.
//
// Two things are load-bearing. An empty 200 is not parseable as a health
// document, so the CLI reads this channel as "nothing is listening" and every
// verifier CRUD call — the code-review sidecar among them — silently stops
// happening. And the marker must NOT be the board's: a client that merely
// inferred this port would otherwise commit the board's token to a peer that
// serves two methods for one task and 404s on the rest.
func TestApprovalServerHealth_AnswersAsTheVerifierControlChannel(t *testing.T) {
	srv := newTestApprovalServer(t)

	resp, err := http.Get("http://" + srv.Addr() + "/health")
	if err != nil {
		t.Fatalf("probe health: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}

	var health struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		HomeID  string `json:"home_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("health body is not a JSON document a client can read: %v", err)
	}
	if health.Status != "ok" {
		t.Errorf("status = %q, want %q", health.Status, "ok")
	}
	if health.Service != VerifierControlServiceMarker {
		t.Errorf("service = %q, want %q", health.Service, VerifierControlServiceMarker)
	}
	if health.Service == httpserve.ServiceMarker {
		t.Errorf("service = %q claims to be a whole board; an inferred client would send it the board token", health.Service)
	}
	// No home is claimed on purpose: this channel is reached only through a
	// target the runner names outright, and a home id here would be compared
	// against the verifier's sandbox home and refused.
	if health.HomeID != "" {
		t.Errorf("home_id = %q, want empty", health.HomeID)
	}
}
