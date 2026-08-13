package workercontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/providerid"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
)

func TestPlacementSharesCapacityAndReleasesOnTerminal(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		service := New(engine.Open(t))
		registerPlacementWorker(t, service, "node-a", []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce", "trusted_work=true", "encrypted_work=true"})
		registerPlacementWorker(t, service, "node-b", []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce", "trusted_work=true", "encrypted_work=true"})

		first := placementRequest(t, "run-place-1", "effect-place-1")
		placed1, err := service.ScheduleStart(t.Context(), first)
		if err != nil || placed1.WorkerID != "node-a" {
			t.Fatalf("first placement = %+v, %v", placed1, err)
		}
		second := placementRequest(t, "run-place-2", "effect-place-2")
		placed2, err := service.ScheduleStart(t.Context(), second)
		if err != nil || placed2.WorkerID != "node-b" {
			t.Fatalf("second placement = %+v, %v", placed2, err)
		}
		third := placementRequest(t, "run-place-3", "effect-place-3")
		if _, err := service.ScheduleStart(t.Context(), third); !errors.Is(err, ErrNoEligibleWorker) {
			t.Fatalf("over-capacity placement = %v", err)
		}
		if _, err := service.AppendEvents(t.Context(), EventBatch{SessionID: placed1.SessionID, Events: []executioncontract.EventEnvelope{event(first.Spec.RunID, 1, executioncontract.EventTerminal)}}); err != nil {
			t.Fatal(err)
		}
		placed3, err := service.ScheduleStart(t.Context(), third)
		if err != nil || placed3.WorkerID != "node-a" {
			t.Fatalf("placement after release = %+v, %v", placed3, err)
		}
		replayed, err := service.ScheduleStart(t.Context(), third)
		if err != nil || replayed.WorkerID != placed3.WorkerID || replayed.Command.Sequence != placed3.Command.Sequence {
			t.Fatalf("idempotent placement = %+v, %v", replayed, err)
		}
	})
}

func TestPlacementIsReachableThroughProductionHandler(t *testing.T) {
	service := New(dbtest.SQLite(t))
	registerPlacementWorker(t, service, "http-node", []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce"})
	request := placementRequest(t, "run-http-place", "effect-http-place")
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/runs/schedule", bytes.NewReader(body))
	service.Handler().ServeHTTP(recorder, httpRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("schedule status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var placed Placement
	if err := json.Unmarshal(recorder.Body.Bytes(), &placed); err != nil || placed.WorkerID != "http-node" {
		t.Fatalf("HTTP placement = %+v, %v", placed, err)
	}
}

func TestPlacementPinsAffinityTrustHealthAndFallback(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		service := New(engine.Open(t))
		registerPlacementWorker(t, service, "healthy", []string{"capacity=4", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce", "trusted_work=true", "encrypted_work=true", "repository=repo", "os=linux", "arch=arm64", "label:pool=secure"})
		registerPlacementWorker(t, service, "unhealthy", []string{"capacity=4", "provider=claude", "provider_health:claude=unavailable", "sandbox=report", "trusted_work=false"})

		request := placementRequest(t, "run-policy", "effect-policy")
		request.NodeOverride, request.WorkType, request.RequireEncrypted = "healthy", "work", true
		request.OS, request.Architecture, request.Sandbox = "linux", "arm64", "enforce"
		request.Labels = map[string]string{"pool": "secure"}
		placed, err := service.ScheduleStart(t.Context(), request)
		if err != nil || placed.WorkerID != "healthy" {
			t.Fatalf("policy placement = %+v, %v", placed, err)
		}

		missingPin := placementRequest(t, "run-pin", "effect-pin")
		missingPin.NodeOverride, missingPin.AllowLocalFallback = "missing", true
		if result, err := service.ScheduleStart(t.Context(), missingPin); !errors.Is(err, ErrNoEligibleWorker) || result.LocalFallback {
			t.Fatalf("hard pin result = %+v, %v", result, err)
		}

		fallback := placementRequest(t, "run-local", "effect-local")
		fallback.Spec.Provider.Provider = providerid.Codex
		refreshPlacementCommand(t, &fallback)
		fallback.AllowLocalFallback = true
		if result, err := service.ScheduleStart(t.Context(), fallback); err != nil || !result.LocalFallback {
			t.Fatalf("local fallback = %+v, %v", result, err)
		}
	})
}

func TestConcurrentPlacementCannotDuplicateRunOrCapacity(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		service := New(engine.Open(t))
		registerPlacementWorker(t, service, "node", []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce"})
		request := placementRequest(t, "run-concurrent", "effect-concurrent")
		var wg sync.WaitGroup
		results := make(chan Placement, 8)
		errs := make(chan error, 8)
		for range 8 {
			wg.Go(func() {
				result, err := service.ScheduleStart(t.Context(), request)
				results <- result
				errs <- err
			})
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent placement: %v", err)
			}
		}
		for result := range results {
			if result.WorkerID != "node" || result.Command.Sequence != 1 {
				t.Fatalf("concurrent result = %+v", result)
			}
		}
		var count int
		if err := service.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM remote_runs`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("remote run count = %d, %v", count, err)
		}
	})
}

func TestPlacementDrainDisableAndDiagnostics(t *testing.T) {
	dbtest.Each(t, func(t *testing.T, engine dbtest.Engine) {
		t.Helper()
		service := New(engine.Open(t))
		disabled := registerPlacementWorker(t, service, "disabled-node", []string{"capacity=1", "provider=claude", "provider_health:claude=healthy", "sandbox=enforce"})
		if err := service.SetWorkerDisabled(t.Context(), disabled.WorkerID, true); err != nil {
			t.Fatal(err)
		}
		request := placementRequest(t, "run-disabled", "effect-disabled")
		request.NodeOverride = disabled.WorkerID
		result, err := service.ScheduleStart(t.Context(), request)
		if !errors.Is(err, ErrNoEligibleWorker) || len(result.Candidates) != 1 || result.Candidates[0].Reasons[0] != "worker is disabled" {
			t.Fatalf("disabled placement = %+v, %v", result, err)
		}
		if err := service.SetWorkerDisabled(t.Context(), disabled.WorkerID, false); err != nil {
			t.Fatal(err)
		}
		placed, err := service.ScheduleStart(t.Context(), request)
		if err != nil || placed.WorkerID != disabled.WorkerID {
			t.Fatalf("re-enabled placement = %+v, %v", placed, err)
		}
		if err := service.Drain(t.Context(), placed.SessionID); err != nil {
			t.Fatal(err)
		}
		next := placementRequest(t, "run-drained", "effect-drained")
		next.NodeOverride = disabled.WorkerID
		if _, err := service.ScheduleStart(t.Context(), next); !errors.Is(err, ErrNoEligibleWorker) {
			t.Fatalf("draining worker placement = %v", err)
		}
		diagnostics, err := service.Diagnostics(t.Context())
		if err != nil || len(diagnostics) == 0 || diagnostics[0].State != "draining" {
			t.Fatalf("diagnostics = %+v, %v", diagnostics, err)
		}
	})
}

func registerPlacementWorker(t *testing.T, service *Service, worker string, capabilities []string) Session {
	t.Helper()
	session, err := service.Register(t.Context(), RegisterRequest{
		WorkerID: worker, Capabilities: capabilities,
		Negotiation: executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: "worker-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func placementRequest(t *testing.T, runID, effectID string) PlacementRequest {
	t.Helper()
	spec, command := startContract(t, runID, effectID)
	spec.Provider.Provider, spec.Provider.Model = providerid.Claude, "sonnet"
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	command.Payload = payload
	command.SentAt = time.Now().UTC()
	return PlacementRequest{Spec: spec, Command: command, AllowAffinityFallback: true}
}

func refreshPlacementCommand(t *testing.T, request *PlacementRequest) {
	t.Helper()
	payload, err := json.Marshal(executioncontract.StartCommandPayload{Spec: &request.Spec})
	if err != nil {
		t.Fatal(err)
	}
	request.Command.Payload = payload
}
