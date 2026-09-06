package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/sybra"
)

func TestClusterReconcileResultsDefaultsToDryRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []any
	}{
		{"default", nil, []any{false, "", float64(100)}},
		{"apply page", []string{"--apply", "--after", "cursor", "--limit", "2"}, []any{true, "cursor", float64(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan []any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/App/ReconcileRemoteResults" {
					t.Errorf("unexpected route: %s %s", r.Method, r.URL.Path)
				}
				var args []any
				if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
					t.Error(err)
				}
				requests <- args
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(sybra.RemoteResultRecoveryReport{Apply: tc.want[0].(bool), Scanned: 1, Preserved: 1, Reasons: map[string]int{"missing_completion_receipt": 1}})
			}))
			t.Cleanup(srv.Close)
			api := &apiClient{baseURL: srv.URL, token: "fixture", http: srv.Client()}
			code, output := captureStdout(t, func() int {
				return cmdCluster(config.DefaultConfig(), api, append([]string{"reconcile-results"}, tc.args...), true)
			})
			if code != 0 {
				t.Fatalf("exit %d: %s", code, output)
			}
			if got := <-requests; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args = %#v, want %#v", got, tc.want)
			}
			var report sybra.RemoteResultRecoveryReport
			if err := json.Unmarshal([]byte(output), &report); err != nil || report.Preserved != 1 {
				t.Fatalf("invalid JSON report: %s, %v", output, err)
			}
		})
	}
}

func TestClusterReconcileResultsFailsClosedWithoutServerOrValidFlags(t *testing.T) {
	for _, args := range [][]string{nil, {"--limit", "0"}, {"--limit", "101"}, {"unexpected"}, {"--unknown"}} {
		code, _ := captureStdout(t, func() int { return cmdClusterReconcileResults(nil, args, true) })
		if code == 0 {
			t.Fatalf("accepted unavailable server or invalid flags: %v", args)
		}
	}
}
