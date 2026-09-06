package workercontrol

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/db"
	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestCompletionAcknowledgementRequiresExactReceipt(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		service := New(engine.Open(t))
		session := register(t, service, "receipt-worker")
		spec, command := startContract(t, "receipt-run", "receipt-effect")
		if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, command); err != nil {
			t.Fatal(err)
		}
		terminal := event(spec.RunID, 2, executioncontract.EventTerminal)
		terminal.Payload = json.RawMessage(`{"state":"failed","artifactState":"failed","error":"fixture"}`)
		if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{
			event(spec.RunID, 1, executioncontract.EventOutput), terminal,
		}}); err != nil {
			t.Fatal(err)
		}
		receipt := TerminalReceipt(terminal)
		if receipt == "" {
			t.Fatal("valid terminal produced no receipt")
		}
		for _, proof := range []string{"", "v1:different"} {
			if changed, err := service.AcknowledgeCompletedResult(t.Context(), spec.RunID, proof); changed || !errors.Is(err, ErrCompletionUnproven) {
				t.Fatalf("unproven acknowledgement = %v, %v", changed, err)
			}
		}
		pending, err := service.PendingResults(t.Context(), "", 100)
		if err != nil || len(pending) != 1 || pending[0].PendingEvents != 2 {
			t.Fatalf("pending result lost: %+v, %v", pending, err)
		}
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/worker/v1/events/receipt-run/ack",
			strings.NewReader(`{"sessionId":"`+session.SessionID+`","through":2}`)))
		if response.Code != http.StatusGone {
			t.Fatalf("legacy unproven ACK returned %d", response.Code)
		}
		pending, err = service.PendingResults(t.Context(), "", 100)
		if err != nil || len(pending) != 1 || pending[0].PendingEvents != 2 {
			t.Fatalf("legacy ACK changed evidence: %+v, %v", pending, err)
		}
		// Neither a restarted leader nor an expired/replaced session should
		// need to resurrect a worker just to consume its already-durable result.
		service.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
		if _, err := service.Register(t.Context(), RegisterRequest{
			WorkerID: "receipt-worker", ResumeSessionID: session.SessionID,
			Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
		}); err != nil {
			t.Fatal(err)
		}
		restarted := New(engine.Open(t))
		var applied atomic.Int32
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				changed, err := restarted.AcknowledgeCompletedResult(t.Context(), spec.RunID, receipt)
				if err != nil {
					t.Errorf("acknowledge: %v", err)
				}
				if changed {
					applied.Add(1)
				}
			})
		}
		wg.Wait()
		if applied.Load() != 1 {
			t.Fatalf("applied %d times, want one", applied.Load())
		}
		pending, err = restarted.PendingResults(t.Context(), "", 100)
		if err != nil || len(pending) != 0 {
			t.Fatalf("acknowledged result still pending: %+v, %v", pending, err)
		}
		replayed, err := restarted.ReplayEvents(t.Context(), spec.RunID, 0, 100)
		if err != nil || len(replayed) != 2 {
			t.Fatalf("recovery deleted evidence: %d, %v", len(replayed), err)
		}
	})
}

func TestCompletionArtifactAndReceiptFences(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, database *db.DB) {
		t.Helper()
		service := New(database)
		session := register(t, service, "artifact-receipt-worker")
		spec, command := startContract(t, "artifact-receipt-run", "artifact-receipt-effect")
		if _, err := service.Enqueue(t.Context(), session.SessionID, &spec, command); err != nil {
			t.Fatal(err)
		}
		terminal := event(spec.RunID, 1, executioncontract.EventTerminal)
		terminal.Payload = json.RawMessage(`{"state":"succeeded","artifactState":"ready"}`)
		if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: session.SessionID, Events: []executioncontract.EventEnvelope{terminal}}); err != nil {
			t.Fatal(err)
		}
		for _, state := range []string{"pending", "staged", "importing", "imported", "rejected"} {
			if _, err := database.ExecContext(t.Context(), database.Rebind(`UPDATE remote_runs SET artifact_state = ? WHERE run_id = ?`), state, spec.RunID); err != nil {
				t.Fatal(err)
			}
			result, err := service.ResultForRun(t.Context(), spec.RunID)
			if err != nil {
				t.Fatal(err)
			}
			wantReady := state == "imported" || state == "rejected"
			if (result.HoldReason(TerminalReceipt(terminal)) == "") != wantReady {
				t.Fatalf("wrong artifact gate for %s", state)
			}
			if !wantReady {
				if changed, err := service.AcknowledgeCompletedResult(t.Context(), spec.RunID, TerminalReceipt(terminal)); changed || !errors.Is(err, ErrCompletionUnproven) {
					t.Fatalf("unresolved artifact acknowledged: %v, %v", changed, err)
				}
			}
		}
		for _, limit := range []int{-1, 0, 101} {
			if _, err := service.PendingResults(t.Context(), "", limit); err == nil {
				t.Fatal("invalid page limit accepted")
			}
		}
	})
}

func TestTerminalReceiptRejectsMalformedAndBindsIdentity(t *testing.T) {
	terminal := event("receipt-identity", 1, executioncontract.EventTerminal)
	terminal.Payload = json.RawMessage(`{"state":"failed","artifactState":"failed"}`)
	receipt := TerminalReceipt(terminal)
	for _, mutate := range []func(*executioncontract.EventEnvelope){
		func(e *executioncontract.EventEnvelope) { e.RunID = "another-run" },
		func(e *executioncontract.EventEnvelope) { e.Sequence++ },
		func(e *executioncontract.EventEnvelope) {
			e.Payload = json.RawMessage(`{"state":"canceled","artifactState":"failed"}`)
		},
	} {
		changed := terminal
		mutate(&changed)
		if TerminalReceipt(changed) == receipt {
			t.Fatal("receipt did not bind event identity/payload")
		}
	}
	for _, payload := range []string{
		`{`, `{}`, `{"state":"unknown","artifactState":"failed"}`, `{"state":"failed","artifactState":"unknown"}`,
		`{"state":"failed","artifactState":"failed","error":123}`,
		`{"state":"failed","artifactState":"failed","artifactError":123}`,
		`{"state":"failed","artifactState":"failed","permanent":"yes"}`,
		`{"state":"failed","artifactState":"failed","admissionDeferred":"yes"}`,
	} {
		terminal.Payload = json.RawMessage(payload)
		if TerminalReceipt(terminal) != "" {
			t.Fatalf("malformed terminal minted a receipt: %s", payload)
		}
	}
}
