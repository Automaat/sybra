package workerupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/executioncontract"
	"github.com/Automaat/sybra/internal/testutil/dbtest"
	"github.com/Automaat/sybra/internal/workercontrol"
)

const oldSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const newSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fixture struct {
	r                 *runner
	service           *workercontrol.Service
	session           workercontrol.Session
	commands          [][]string
	ghCalls           [][]string
	rejectAttestation bool
	failRestart       bool
	noRegistration    bool
	noDiagnostics     bool
	lostFinish        bool
	clock             time.Time
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{WorkerID: "test-worker", LeaderHomeID: strings.Repeat("a", 64), TokenEnv: "TEST_UPDATE_TOKEN", Repository: "example/project", ReleaseRoot: filepath.Join(dir, "releases"), StateDir: filepath.Join(dir, "state"), CurrentLink: filepath.Join(dir, "current"), AgentConfig: filepath.Join(dir, "agent.yaml"), ServiceUser: "sybra"}
	t.Setenv(cfg.TokenEnv, "private-test-token")
	for _, path := range []string{cfg.StateDir, filepath.Join(cfg.ReleaseRoot, oldSHA)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.ReleaseRoot, oldSHA, "sybra-agentd"), []byte("retained"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cfg.ReleaseRoot, oldSHA), cfg.CurrentLink); err != nil {
		t.Fatal(err)
	}
	f := &fixture{service: workercontrol.New(dbtest.SQLite(t)), clock: time.Now()}
	f.service.SetUpdateRevision(newSHA)
	f.register(t, oldSHA)
	handler := f.service.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health" {
			if request.Header.Get("Authorization") != "" {
				t.Error("credential leaked into identity probe")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"service": "sybra", "home_id": cfg.LeaderHomeID, "status": "ok"})
			return
		}
		if request.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("missing bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.noDiagnostics && request.URL.Path == "/worker/v1/diagnostics" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		if f.lostFinish && request.URL.Path == "/worker/v1/update/finish" {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Errorf("finish before lost reply: %d", recorder.Code)
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		handler.ServeHTTP(w, request)
	}))
	t.Cleanup(server.Close)
	cfg.LeaderURL = server.URL
	f.r = &runner{cfg: cfg, leader: newLeaderClient(cfg), now: func() time.Time { return f.clock }, trust: func(string) error { return nil }, serviceCheck: func(context.Context) error { return nil }, localCheck: func(context.Context, string) error { return nil }}
	f.r.gh = func(_ context.Context, args ...string) ([]byte, error) {
		f.ghCalls = append(f.ghCalls, slices.Clone(args))
		switch strings.Join(args[:2], " ") {
		case "run list":
			return []byte(`[{"databaseId":123,"headSha":"` + newSHA + `","headBranch":"main","event":"push","conclusion":"success"}]`), nil
		case "run download":
			stage := args[len(args)-1]
			for _, name := range []string{"sybra-agentd", "sybra-worker-update"} {
				if err := os.WriteFile(filepath.Join(stage, name), []byte("verified-fixture"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			return nil, nil
		case "attestation verify":
			if f.rejectAttestation {
				return nil, errors.New("untrusted provenance")
			}
			for _, pair := range [][2]string{{"--repo", cfg.Repository}, {"--signer-workflow", cfg.Repository + "/.github/workflows/ci.yml"}, {"--source-ref", "refs/heads/main"}, {"--source-digest", newSHA}, {"--signer-digest", newSHA}} {
				i := slices.Index(args, pair[0])
				if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
					t.Errorf("missing provenance binding %v in %v", pair, args)
				}
			}
			if !slices.Contains(args, "--deny-self-hosted-runners") {
				t.Error("self-hosted provenance accepted")
			}
			return nil, nil
		default:
			t.Fatalf("unexpected GitHub command %v", args)
			return nil, nil
		}
	}
	f.r.command = func(_ context.Context, name string, args ...string) error {
		f.commands = append(f.commands, append([]string{name}, args...))
		if name == "/usr/sbin/runuser" {
			if len(args) != 7 || args[0] != "-u" || args[1] != "sybra" || args[4] != "-check-config" {
				t.Fatalf("unsafe preflight %v", args)
			}
			return nil
		}
		if name != "/usr/bin/systemctl" || !slices.Equal(args, []string{"restart", "sybra-agentd.service"}) {
			t.Fatalf("unexpected host action %s %v", name, args)
		}
		if f.failRestart {
			return errors.New("restart failed")
		}
		if !f.noRegistration {
			revision, err := f.r.pointer()
			if err != nil {
				t.Fatal(err)
			}
			f.register(t, revision)
		}
		return nil
	}
	return f
}

func (f *fixture) register(t *testing.T, revision string) {
	t.Helper()
	session, err := f.service.Register(t.Context(), workercontrol.RegisterRequest{
		WorkerID: "test-worker", ResumeSessionID: f.session.SessionID,
		Negotiation:  executioncontract.Negotiation{ProtocolMin: executioncontract.CurrentVersion(), ProtocolMax: executioncontract.CurrentVersion(), BuildVersion: revision},
		Capabilities: []string{"capacity=2", "readiness=ready", "buffered_events=0", "pending_artifacts=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.session = session
}

func TestUpdateEndToEndAndLostFinishReply(t *testing.T) {
	f := setup(t)
	var j journal
	if status, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "verifying" {
		t.Fatalf("start: %s %+v %v", status, j, err)
	}
	if err := f.service.SetWorkerDisabled(t.Context(), f.session.WorkerID, true); err != nil {
		t.Fatal(err)
	}
	f.lostFinish = true
	if _, err := f.r.step(t.Context(), &j, false); err == nil {
		t.Fatal("expected lost finish reply")
	}
	f.lostFinish = false
	// Simulate a fresh updater process reading its persisted intent.
	reloaded, err := f.r.load()
	if err != nil {
		t.Fatal(err)
	}
	if status, err := f.r.step(t.Context(), &reloaded, false); err != nil || status != "complete" {
		t.Fatalf("recover finish: %s %v", status, err)
	}
	current, err := f.r.leader.current(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if current.UpdateHeld || current.State != "disabled" || current.BuildVersion != newSHA {
		t.Fatalf("operator state lost: %+v", current)
	}
	if len(f.commands) != 2 {
		t.Fatalf("extra restart on lost reply: %v", f.commands)
	}
}

func TestRejectProvenanceBeforeHoldingOrExecuting(t *testing.T) {
	f := setup(t)
	f.rejectAttestation = true
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err == nil {
		t.Fatal("accepted untrusted release")
	}
	if len(f.commands) != 0 || j.Phase != "" {
		t.Fatalf("unverified code executed or held: %v %+v", f.commands, j)
	}
	current, err := f.r.leader.current(t.Context())
	if err != nil || current.UpdateHeld {
		t.Fatalf("untrusted artifact created hold: %+v %v", current, err)
	}
}

func TestRollbackQuarantinesFailedCandidate(t *testing.T) {
	f := setup(t)
	f.failRestart = true
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err == nil || j.Phase != "rollback" {
		t.Fatalf("failed activation: %+v %v", j, err)
	}
	f.failRestart = false
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "rollback-verifying" {
		t.Fatalf("rollback: %+v %v", j, err)
	}
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "quarantined" {
		t.Fatalf("rollback health: %+v %v", j, err)
	}
	before := len(f.commands)
	if _, err := f.r.step(t.Context(), &j, false); err != nil || len(f.commands) != before {
		t.Fatalf("quarantine retried automatically: %v", err)
	}
	revision, err := f.r.pointer()
	if err != nil || revision != oldSHA {
		t.Fatalf("rollback pointer %s %v", revision, err)
	}
	if _, err := os.Stat(filepath.Join(f.r.cfg.ReleaseRoot, newSHA, "sybra-agentd")); err != nil {
		t.Fatal("failed candidate was not retained", err)
	}
}

func TestCrashAfterRestartBeforeJournalAndNoRegistration(t *testing.T) {
	f := setup(t)
	f.noRegistration = true
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err != nil {
		t.Fatal(err)
	}
	j.Phase = "switching" // durable state immediately before the restart
	if err := f.r.save(&j); err != nil {
		t.Fatal(err)
	}
	f.noDiagnostics = true
	f.clock = f.clock.Add(3 * time.Minute)
	f.noRegistration = false
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "rollback-verifying" {
		t.Fatalf("stranded switching recovery: %+v %v", j, err)
	}
	f.noDiagnostics = false
	if _, err := f.r.step(t.Context(), &j, false); err != nil || j.Phase != "quarantined" {
		t.Fatalf("rollback completion: %+v %v", j, err)
	}
}

func TestCrashAfterSuccessfulRestartDoesNotRestartAgain(t *testing.T) {
	f := setup(t)
	var j journal
	if _, err := f.r.step(t.Context(), &j, false); err != nil {
		t.Fatal(err)
	}
	j.Phase = "switching"
	before := len(f.commands)
	if _, err := f.r.step(t.Context(), &j, false); err != nil || len(f.commands) != before {
		t.Fatalf("duplicate restart: %v %v", f.commands, err)
	}
}
