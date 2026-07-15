package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func healthServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRosterRejectsDuplicateAndUnnamed(t *testing.T) {
	if _, err := NewRoster([]Node{{Name: ""}}, nil); err == nil {
		t.Error("want error for an unnamed node")
	}
	if _, err := NewRoster([]Node{{Name: "a"}, {Name: "a"}}, nil); err == nil {
		t.Error("want error for a duplicate node name")
	}
}

func TestRosterProbeStatuses(t *testing.T) {
	live := healthServer(t)

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	roster, err := NewRoster([]Node{
		{Name: "online", Endpoints: []string{live.URL}},
		{Name: "degraded", Endpoints: []string{deadURL, live.URL}},
		{Name: "offline", Endpoints: []string{deadURL}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	roster.ProbeAll(context.Background(), now)
	byName := map[string]NodeHealth{}
	for _, h := range roster.Health() {
		byName[h.Name] = h
	}

	if byName["online"].Status != StatusOnline {
		t.Errorf("online node status = %s", byName["online"].Status)
	}
	if byName["degraded"].Status != StatusDegraded {
		t.Errorf("degraded node status = %s (want degraded — primary down, fallback up)", byName["degraded"].Status)
	}
	if byName["degraded"].ActiveEndpoint != live.URL {
		t.Errorf("degraded active endpoint = %q, want fallback %q", byName["degraded"].ActiveEndpoint, live.URL)
	}
	if byName["offline"].Status != StatusOffline {
		t.Errorf("offline node status = %s", byName["offline"].Status)
	}

	online := roster.OnlineNames()
	if len(online) != 2 {
		t.Errorf("OnlineNames = %v, want online+degraded", online)
	}
}

func TestRosterSetHealthFromEvent(t *testing.T) {
	roster, err := NewRoster([]Node{{Name: "n1", Endpoints: []string{"http://x"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	roster.SetHealthFromEvent("n1", true, now)
	if got := statusOf(t, roster); got != StatusOnline {
		t.Errorf("after online event status = %s, want online", got)
	}
	roster.SetHealthFromEvent("n1", false, now)
	if got := statusOf(t, roster); got != StatusOffline {
		t.Errorf("after drop event status = %s, want offline", got)
	}
}

func statusOf(t *testing.T, roster *Roster) Status {
	t.Helper()
	h := roster.Health()
	if len(h) == 0 {
		t.Fatal("roster has no health entries")
	}
	return h[0].Status
}
