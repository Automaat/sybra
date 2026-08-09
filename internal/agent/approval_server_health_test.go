package agent

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Automaat/sybra/internal/httpserve"
)

// TestApprovalServerHealth_AnswersAsASybraControlPlane pins the body a
// verifier's own CLI probes before it will send its task-scoped token. An
// empty 200 is not parseable as a health document, so the CLI reads the
// control channel as "nothing is listening" and every verifier CRUD call —
// the code-review sidecar among them — silently stops happening.
func TestApprovalServerHealth_AnswersAsASybraControlPlane(t *testing.T) {
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
	if health.Service != httpserve.ServiceMarker {
		t.Errorf("service = %q, want %q", health.Service, httpserve.ServiceMarker)
	}
	// No home is claimed on purpose: this channel fronts two methods for one
	// task, and a home id here would make an inferred-target check compare the
	// verifier's sandbox home against the operator's and refuse.
	if health.HomeID != "" {
		t.Errorf("home_id = %q, want empty", health.HomeID)
	}
}
