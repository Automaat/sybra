package workerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIdentityProbeNeverSendsCredentialToWrongBoard(t *testing.T) {
	f := setup(t)
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		if r.Header.Get("Authorization") != "" {
			t.Error("credential sent to wrong board")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"service": "sybra", "status": "ok", "home_id": strings.Repeat("c", 64)})
	}))
	defer server.Close()
	f.r.leader.cfg.LeaderURL = server.URL
	if _, err := f.r.leader.current(t.Context()); err == nil || seen != 1 {
		t.Fatalf("wrong board accepted: calls=%d err=%v", seen, err)
	}
}

func TestIdentityRedirectRefused(t *testing.T) {
	f := setup(t)
	visited := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { visited = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	f.r.leader.cfg.LeaderURL = redirect.URL
	if _, err := f.r.leader.current(t.Context()); err == nil || visited {
		t.Fatalf("identity redirect followed: %v %v", visited, err)
	}
}

func TestSpoolProofRejectsStaleHeartbeatWindow(t *testing.T) {
	for _, tc := range []struct {
		name, spool string
		safe        bool
	}{
		{"explicit node config empty spool node", `{"sessionId":"live"}`, true},
		{"empty event buckets", `{"sessionId":"live","events":{"run":[]}}`, true},
		{"queued artifact after terminal", `{"sessionId":"live","artifacts":{"manifest":{}}}`, false},
		{"unacked terminal", `{"sessionId":"live","events":{"run":[{}]}}`, false},
		{"provider still owned", `{"sessionId":"live","runAgents":{"run":"agent"}}`, false},
		{"wrong node", `{"nodeId":"other","sessionId":"live"}`, false},
		{"wrong session", `{"sessionId":"other"}`, false},
		{"missing identity", `{}`, false},
		{"truncated", `{"sessionId":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSpool([]byte(tc.spool), "worker", "live")
			if (err == nil) != tc.safe {
				t.Fatalf("safe=%v err=%v", tc.safe, err)
			}
		})
	}
}

func TestServiceIdentityIsExact(t *testing.T) {
	r := &runner{cfg: Config{CurrentLink: "/opt/sybra/worker-current", AgentConfig: "/etc/sybra/agent.yaml"}}
	valid := "MainPID=123\nExecStart={ path=/usr/bin/env ; argv[]=/usr/bin/env PATH=/usr/bin /opt/sybra/worker-current/sybra-agentd -config /etc/sybra/agent.yaml ; ignore_errors=no ; }\n"
	if pid, err := r.serviceIdentity(valid); err != nil || pid != 123 {
		t.Fatalf("valid unit %d %v", pid, err)
	}
	for _, bad := range []string{strings.ReplaceAll(valid, "/etc/sybra/agent.yaml", "/etc/sybra/other.yaml"), strings.ReplaceAll(valid, "worker-current", "current"), strings.ReplaceAll(valid, "MainPID=123", "MainPID=bad"), strings.ReplaceAll(valid, "/usr/bin/env PATH=/usr/bin", "/opt/wrapper")} {
		if _, err := r.serviceIdentity(bad); err == nil {
			t.Fatal("accepted another unit command", bad)
		}
	}
}

func TestLostReleaseCannotRollBackAfterLeaseExpires(t *testing.T) {
	f := setup(t)
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err != nil {
		t.Fatal(err)
	}
	f.lostFinish = true
	if _, err := f.r.step(t.Context(), &j, false); err == nil || j.Phase != "releasing" {
		t.Fatalf("missing durable release intent: %+v %v", j, err)
	}
	f.lostFinish = false
	f.noDiagnostics = true
	f.clock = f.clock.Add(5 * time.Minute)
	before := len(f.commands)
	for range 3 {
		if _, err := f.r.step(t.Context(), &j, false); err == nil {
			t.Fatal("expected unavailable session")
		}
	}
	if len(f.commands) != before || j.Phase != "releasing" {
		t.Fatalf("rollback after committed release: %+v %v", j, f.commands)
	}
	f.noDiagnostics = false
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "complete" {
		t.Fatalf("recovered release: %+v %v", j, err)
	}
}

func TestLocalBacklogPreventsRestart(t *testing.T) {
	f := setup(t)
	var j journal
	f.r.localCheck = func(context.Context, string) error { return errors.New("pending local handback") }
	if _, err := f.r.step(t.Context(), &j, false); err == nil {
		t.Fatal("ignored local handback")
	}
	if len(f.commands) != 1 {
		t.Fatalf("restarted with pending local handback: %v", f.commands)
	}
	current, err := f.r.leader.current(t.Context())
	if err != nil || !current.UpdateHeld {
		t.Fatalf("hold not preserved: %+v %v", current, err)
	}
}

func TestPreflightFailureRetainsOneStage(t *testing.T) {
	f := setup(t)
	var j journal
	f.r.command = func(context.Context, string, ...string) error { return errors.New("bad local config") }
	for range 3 {
		if _, err := f.r.step(t.Context(), &j, false); err == nil {
			t.Fatal("preflight failure ignored")
		}
	}
	entries, err := os.ReadDir(f.r.cfg.ReleaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unbounded stages: %v", entries)
	}
	downloads := 0
	for _, call := range f.ghCalls {
		if call[0] == "run" && call[1] == "download" {
			downloads++
		}
	}
	if downloads != 1 || j.Phase != "" {
		t.Fatalf("preflight redownloaded or acquired hold: %d %+v", downloads, j)
	}
}

func TestJournalAndPointerRejectOtherDeployment(t *testing.T) {
	f := setup(t)
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err != nil {
		t.Fatal(err)
	}
	j.WorkerID = "other"
	if err := f.r.save(&j); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.load(); err == nil {
		t.Fatal("accepted another worker journal")
	}
	if err := os.Remove(f.r.cfg.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), newSHA), f.r.cfg.CurrentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.pointer(); err == nil {
		t.Fatal("accepted pointer outside release root")
	}
}

func TestSupersededIntentDoesNotStrandUpdates(t *testing.T) {
	f := setup(t)
	var j journal
	gh := f.r.gh
	f.r.gh = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := gh(ctx, args...)
		if args[0] == "attestation" {
			f.service.SetUpdateRevision(strings.Repeat("c", 40))
		}
		return data, err
	}
	if status, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "retired" {
		t.Fatalf("superseded intent: %s %+v %v", status, j, err)
	}
	current, err := f.r.leader.current(t.Context())
	if err != nil || current.UpdateHeld {
		t.Fatalf("obsolete intent took hold: %+v %v", current, err)
	}
	status, _ := f.r.step(t.Context(), &j, false)
	if status != "waiting for verified artifact" {
		t.Fatalf("did not consider newer leader target: %s", status)
	}
}

func TestConfigRejectsUnsafeDeploymentInputs(t *testing.T) {
	f := setup(t)
	for _, mutate := range []func(*Config){
		func(c *Config) { c.LeaderURL = "http://example.com" },
		func(c *Config) { c.LeaderURL = "https://user:secret@example.com" },
		func(c *Config) { c.ServiceUser = "root" },
		func(c *Config) { c.ReleaseRoot = "/" },
		func(c *Config) { c.StateDir = c.ReleaseRoot },
		func(c *Config) { c.CurrentLink = filepath.Join(c.ReleaseRoot, "current") },
		func(c *Config) { c.LeaderHomeID = "unknown" },
	} {
		cfg := f.r.cfg
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
	if err := trustedPath(t.TempDir()); err == nil {
		t.Fatal("trusted a non-root or shared-temp deployment path")
	}
}

func TestOperatorDisableDuringStageOnlyPostponesUpdate(t *testing.T) {
	f := setup(t)
	var j journal
	gh := f.r.gh
	f.r.gh = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := gh(ctx, args...)
		if args[0] == "attestation" {
			if disableErr := f.service.SetWorkerDisabled(ctx, f.session.WorkerID, true); disableErr != nil {
				t.Fatal(disableErr)
			}
		}
		return data, err
	}
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "retired" {
		t.Fatalf("disabled while staging: %+v %v", j, err)
	}
	f.r.gh = gh
	if err := f.service.SetWorkerDisabled(t.Context(), f.session.WorkerID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "verifying" {
		t.Fatalf("enable did not resume update: %+v %v", j, err)
	}
}
