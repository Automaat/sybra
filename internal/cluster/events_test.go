package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "tok"})
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

func TestSubscribeMultiLineAndCRLF(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: multi\ndata: line1\ndata: line2\n\n")
		_, _ = fmt.Fprint(w, "event: crlf\r\ndata: {\"k\":\"v\"}\r\n\r\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}})
	ch, err := client.Subscribe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	multi := recvEvent(t, ch)
	if multi.Name != "multi" || multi.Data != "line1\nline2" {
		t.Fatalf("multi-line frame = %+v, want data 'line1\\nline2'", multi)
	}
	crlf := recvEvent(t, ch)
	if crlf.Name != "crlf" || crlf.Data != `{"k":"v"}` {
		t.Fatalf("CRLF frame = %+v", crlf)
	}
}

func TestSubscribeFailover(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: ok\ndata: 1\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	live := httptest.NewServer(mux)
	t.Cleanup(live.Close)

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{deadURL, live.URL}})
	ch, err := client.Subscribe(t.Context())
	if err != nil {
		t.Fatalf("Subscribe should fail over to the live endpoint: %v", err)
	}
	if ev := recvEvent(t, ch); ev.Name != "ok" {
		t.Fatalf("event = %+v", ev)
	}
	if client.ActiveEndpoint() != live.URL {
		t.Errorf("active endpoint = %q, want %q", client.ActiveEndpoint(), live.URL)
	}
}

func TestSubscribeOverPinnedTLS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = fmt.Fprint(w, "event: secure\ndata: 1\n\n")
		flusher.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	sum := sha256.Sum256(srv.Certificate().Raw)
	client := mustClient(t, Node{Name: "tls", Endpoints: []string{srv.URL}, TLSPin: hex.EncodeToString(sum[:])})
	ch, err := client.Subscribe(t.Context())
	if err != nil {
		t.Fatalf("SSE over correctly-pinned TLS should connect: %v", err)
	}
	if ev := recvEvent(t, ch); ev.Name != "secure" {
		t.Fatalf("event = %+v", ev)
	}

	wrong := mustClient(t, Node{Name: "tls", Endpoints: []string{srv.URL}, TLSPin: strings.Repeat("00", 32)})
	if _, err := wrong.Subscribe(t.Context()); err == nil {
		t.Fatal("SSE over a wrong pin must reject")
	}
}

func TestSubscribeTokenRejected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := mustClient(t, Node{Name: "n1", Endpoints: []string{srv.URL}, Token: "bad"})
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
