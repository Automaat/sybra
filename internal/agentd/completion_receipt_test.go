package agentd

import (
	"encoding/json"
	"testing"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/workercontrol"
)

func TestAdmissionFailureHasSettledArtifactDisposition(t *testing.T) {
	d := registrationTestDaemon(t, "http://127.0.0.1:1")
	for _, key := range []string{"admissionDeferred", "permanent"} {
		if err := d.emitAdmissionFailure(key, map[string]any{"state": executioncontract.TerminalFailed, key: true, "error": "fixture refusal"}); err != nil {
			t.Fatal(err)
		}
		events := d.spool.snapshot().Events[key]
		if len(events) != 1 || workercontrol.TerminalReceipt(events[0]) == "" {
			t.Fatalf("refusal cannot be durably consumed: %+v", events)
		}
		var payload struct {
			ArtifactState executioncontract.ArtifactState `json:"artifactState"`
		}
		if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload.ArtifactState != executioncontract.ArtifactsFailed {
			t.Fatalf("refusal artifact disposition = %+v, %v", payload, err)
		}
	}
}
