package cluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSubscribeDecodesFrames(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = fmt.Fprint(w, ": heartbeat\n\n")
		_, _ = fmt.Fprint(w, "event: task:updated\ndata: {\"id\":\"t1\"}\n\n")
		_, _ = fmt.Fprint(w, "event: agent:state\ndata: {\"state\":\"running\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, _ := NewClient(Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "tok"}, nil)
	ch, err := client.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	first := recvEvent(t, ch)
	if first.Name != "task:updated" || first.Data != `{"id":"t1"}` {
		t.Fatalf("first event = %+v", first)
	}
	second := recvEvent(t, ch)
	if second.Name != "agent:state" || second.Data != `{"state":"running"}` {
		t.Fatalf("second event = %+v", second)
	}
}

func TestSubscribeTokenRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, _ := NewClient(Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "bad"}, nil)
	if _, err := client.Subscribe(context.Background()); err == nil {
		t.Fatal("want error when /events rejects the token")
	}
}

func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
		return Event{}
	}
}
