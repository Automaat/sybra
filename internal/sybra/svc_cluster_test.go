package sybra

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Automaat/sybra/internal/agent"
	"github.com/Automaat/sybra/internal/cluster"
)

func TestAgentNodeStampSurvivesJSON(t *testing.T) {
	a := &agent.Agent{ID: "a1", Node: "pet-box"}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Node != "pet-box" {
		t.Fatalf("node stamp dropped on the wire: %s", raw)
	}
}

func TestRelayFollowerError(t *testing.T) {
	const secret = "SECRET internals"
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
		wantLeak   bool
	}{
		{
			name:       "follower 400 relays its own reason",
			err:        &cluster.APIError{Status: 400, Message: "task is not waiting for human action"},
			wantStatus: 400,
			wantMsg:    "task is not waiting for human action",
		},
		{
			name:       "follower 409 relays as conflict",
			err:        &cluster.APIError{Status: 409, Message: "already running"},
			wantStatus: 409,
			wantMsg:    "already running",
		},
		{
			name:     "follower 500 does not leak follower internals",
			err:      &cluster.APIError{Status: 500, Message: secret},
			wantLeak: true,
		},
		{
			name:     "transport failure does not leak the follower address",
			err:      errors.New("dial tcp " + secret + ":443: connection refused"),
			wantLeak: true,
		},
	}
	svc := &ClusterService{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.relayFollowerError("pet-box", tc.err)
			if got == nil {
				t.Fatal("want error, got nil")
			}
			var ce *clientError
			isClient := errors.As(got, &ce)

			if tc.wantLeak {
				if isClient {
					t.Fatalf("non-4xx surfaced as a client error, so the handler would print it verbatim: %v", got)
				}
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("follower internals leaked in the error text (Wails surfaces it verbatim): %v", got)
				}
				if errors.Is(got, tc.err) {
					t.Fatalf("wrapped error still unwraps to the follower error, so %%v on the cause leaks it: %v", got)
				}
				return
			}

			if !isClient {
				t.Fatalf("follower 4xx must reach the caller as a client error, got %v", got)
			}
			if ce.HTTPStatus() != tc.wantStatus {
				t.Fatalf("status = %d, want %d", ce.HTTPStatus(), tc.wantStatus)
			}
			if ce.Error() != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", ce.Error(), tc.wantMsg)
			}
		})
	}
}

func TestRelayFollowerErrorNilIsNil(t *testing.T) {
	svc := &ClusterService{}
	if err := svc.relayFollowerError("pet-box", nil); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}
