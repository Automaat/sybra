package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestLeaderClientDecodesPerRunEventAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/worker/v1/events" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"acknowledgedThrough": map[string]uint64{"run": 7},
		})
	}))
	t.Cleanup(server.Close)
	client := newLeaderClient(server.URL, "token")
	ack, err := client.events(context.Background(), workercontrol.EventBatch{
		SessionID: "session",
		Events: []executioncontract.EventEnvelope{{
			Version: executioncontract.CurrentVersion(), BuildVersion: "test", RunID: "run", Sequence: 7,
			EventID: "event", IdempotencyKey: "run:7", Type: executioncontract.EventOutput, ObservedAt: time.Now().UTC(),
		}},
	})
	if err != nil || ack != 7 {
		t.Fatalf("events ack = %d, %v", ack, err)
	}
}
