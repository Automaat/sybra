package workerupdate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestCurrentUsesLeaderClockForLeaseFreshness(t *testing.T) {
	cfg := Config{WorkerID: "worker", LeaderHomeID: strings.Repeat("a", 64), TokenEnv: "UPDATE_CLOCK_TEST_TOKEN"}
	t.Setenv(cfg.TokenEnv, "test-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]string{"service": "sybra", "home_id": cfg.LeaderHomeID, "status": "ok"})
			return
		}
		if r.URL.Query().Get("workerId") != cfg.WorkerID {
			t.Error("unscoped diagnostics request")
		}
		// Leader is an hour behind this host; its scoped endpoint already
		// filtered out expired sessions using its own authoritative clock.
		_ = json.NewEncoder(w).Encode([]workercontrol.Diagnostics{{WorkerID: cfg.WorkerID, SessionID: "live-session", State: "active", LeaseExpiresAt: time.Now().Add(-time.Hour)}})
	}))
	t.Cleanup(server.Close)
	cfg.LeaderURL = server.URL
	got, err := newLeaderClient(cfg).current(t.Context())
	if err != nil || got.SessionID != "live-session" {
		t.Fatalf("leader-live session rejected under clock skew: %+v %v", got, err)
	}
}
