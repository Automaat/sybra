package clusterlead

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Automaat/sybra/internal/config"
	"github.com/Automaat/sybra/internal/task"
)

func workConfig(followerURL string, trusted bool, tlsPin string) *config.Config {
	return &config.Config{Cluster: config.ClusterConfig{
		Role: config.ClusterRoleLeader,
		Followers: []config.Follower{{
			Name:      "box",
			Endpoints: []string{followerURL},
			Trusted:   trusted,
			TLSPin:    tlsPin,
			Homes:     []string{"owner/work"},
		}},
	}}
}

func isWork(id string) bool { return id == "owner/work" }

func routeWork(t *testing.T, cfg *config.Config) (mgr *task.Manager, audited []string) {
	t.Helper()
	roster, err := NewRoster(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr = newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, isWork, func(taskID, _, _ string) { audited = append(audited, taskID) }, nil)
	if _, _, err := mgr.Put(task.Task{ID: "w1", Title: "confidential", Status: task.StatusTodo, ProjectID: "owner/work"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := mgr.Get("w1")
	_, _ = assigner.Route(context.Background(), cur)
	return mgr, audited
}

func TestWorkTaskBlockedFromUntrustedNode(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := workConfig(srv.URL, false, "")

	mgr, audited := routeWork(t, cfg)

	if _, ok := stub.lastAssigned(); ok {
		t.Fatal("work task was pushed to an UNTRUSTED follower — confidentiality breach")
	}
	got, _ := mgr.Get("w1")
	if got.Status != task.StatusBlocked {
		t.Errorf("work task status = %s, want blocked", got.Status)
	}
	if got.StatusReason == "" {
		t.Error("blocked task must carry a status reason pointer")
	}
	if len(audited) != 1 {
		t.Errorf("audit events = %d, want 1", len(audited))
	}
}

func TestWorkTaskBlockedFromCleartextNode(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := workConfig(srv.URL, true, "")

	mgr, audited := routeWork(t, cfg)

	if _, ok := stub.lastAssigned(); ok {
		t.Fatal("work task was pushed over PLAIN HTTP even though the node is trusted — confidentiality breach")
	}
	if got, _ := mgr.Get("w1"); got.Status != task.StatusBlocked {
		t.Errorf("work task over cleartext should be blocked, got %s", got.Status)
	}
	if len(audited) != 1 {
		t.Errorf("audit events = %d, want 1", len(audited))
	}
}

func TestWorkTaskRoutesToTrustedEncryptedNode(t *testing.T) {
	stub := &followerStub{}
	srv := stub.tlsServer(t)
	sum := sha256.Sum256(srv.Certificate().Raw)
	cfg := workConfig(srv.URL, true, hex.EncodeToString(sum[:]))

	mgr, audited := routeWork(t, cfg)

	got, ok := stub.lastAssigned()
	if !ok || got.ID != "w1" {
		t.Fatalf("work task should be pushed to a trusted, encrypted node: %+v", got)
	}
	if canonical, _ := mgr.Get("w1"); canonical.Status == task.StatusBlocked {
		t.Error("a trusted+encrypted route must not block the task")
	}
	if len(audited) != 0 {
		t.Errorf("no confidentiality block expected, got %d audit events", len(audited))
	}
}

func TestPetTaskRoutesRegardlessOfTrust(t *testing.T) {
	stub := &followerStub{}
	srv := stub.server(t)
	cfg := &config.Config{Cluster: config.ClusterConfig{
		Role:      config.ClusterRoleLeader,
		Followers: []config.Follower{{Name: "box", Endpoints: []string{srv.URL}, Trusted: false, Homes: []string{"owner/pet"}}},
	}}
	roster, _ := NewRoster(cfg, nil)
	mgr := newManager(t)
	assigner := NewAssigner(cfg, mgr, roster, isWork, nil, nil)
	if _, _, err := mgr.Put(task.Task{ID: "p1", Title: "pet", Status: task.StatusTodo, ProjectID: "owner/pet"}); err != nil {
		t.Fatal(err)
	}
	cur, _ := mgr.Get("p1")
	routed, err := assigner.Route(context.Background(), cur)
	if err != nil || !routed {
		t.Fatalf("pet task should route to an untrusted cleartext node: routed=%v err=%v", routed, err)
	}
	if _, ok := stub.lastAssigned(); !ok {
		t.Error("pet task was not pushed")
	}
}

func (f *followerStub) tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("POST /api/{service}/{method}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/api/TaskService/AssignTask" {
			var args []task.Task
			_ = json.Unmarshal(body, &args)
			if len(args) == 1 {
				f.mu.Lock()
				f.assigned = append(f.assigned, args[0])
				f.mu.Unlock()
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
