package sybra

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/httpapi"
)

func TestVerifierControlRefusesWritesWhileDraining(t *testing.T) {
	var a App
	a.lifecycle.Store(uint32(lifecycleStateRunning))
	if err := a.HTTPAdmission("TaskService", "UpdateTask", httpapi.MethodMeta{}); err != nil {
		t.Fatalf("a running board refused a verifier write: %v", err)
	}

	a.BeginDrain()

	err := a.HTTPAdmission("TaskService", "UpdateTask", httpapi.MethodMeta{})
	if err == nil {
		t.Fatal("a draining board accepted a verifier write, so its follow-up lands behind a wait group shutdown is already parked on")
	}
	var lifecycleErr lifecycleHTTPError
	if !errors.As(err, &lifecycleErr) || lifecycleErr.status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 the agent CLI retries", err)
	}
	if readErr := a.HTTPAdmission("TaskService", "GetTask", httpapi.MethodMeta{ReadOnly: true}); readErr != nil {
		t.Fatalf("a draining board refused a read: %v", readErr)
	}
}

func TestVerifierControlMuxCarriesTheDrainGuard(t *testing.T) {
	a := &App{logger: discardLogger()}
	a.taskSvc = &TaskService{}
	a.lifecycle.Store(uint32(lifecycleStateRunning))
	mux := a.verifierControlMux()

	a.BeginDrain()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/TaskService/UpdateTask", strings.NewReader(`["t1",{}]`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — the control channel accepted a write from a draining board", rec.Code)
	}
}
