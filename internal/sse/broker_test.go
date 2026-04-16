package sse_test

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Automaat/sybra/internal/sse"
)

func TestBroker_EmitDelivers(t *testing.T) {
	b := sse.New()
	ch, cancel := b.Subscribe("test.event")
	defer cancel()

	b.Emit("test.event", map[string]string{"key": "value"})

	select {
	case msg := <-ch:
		if !strings.Contains(msg, "key") {
			t.Fatalf("unexpected message: %s", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestBroker_EmitNoSubscribers(t *testing.T) {
	b := sse.New()
	// Must not panic when no subscribers registered.
	b.Emit("no.subs", "payload")
}

func TestBroker_SlowConsumerDropped(t *testing.T) {
	b := sse.New()
	ch, cancel := b.Subscribe("drop.test")
	defer cancel()

	// Fill the channel buffer.
	for range 256 {
		b.Emit("drop.test", "msg")
	}
	// This extra emit must not block.
	done := make(chan struct{})
	go func() {
		b.Emit("drop.test", "overflow")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit blocked on slow consumer")
	}
	_ = ch
}

func TestBroker_CancelUnsubscribes(t *testing.T) {
	b := sse.New()
	_, cancel := b.Subscribe("cancel.test")
	cancel()
	// After cancel, emit must not panic or block.
	b.Emit("cancel.test", "after-cancel")
}

func TestBroker_ServeAll(t *testing.T) {
	b := sse.New()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /events", b.ServeAll)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	type result struct {
		eventType string
		data      string
	}
	resultCh := make(chan result, 2)

	go func() {
		resp, err := http.Get(srv.URL + "/events")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var pending result
		received := 0
		for sc.Scan() {
			line := sc.Text()
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				pending.eventType = name
			} else if data, ok := strings.CutPrefix(line, "data: "); ok {
				pending.data = data
				resultCh <- pending
				pending = result{}
				received++
				if received == 2 {
					return // close body so srv.Close() doesn't block
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	b.Emit("issues:updated", map[string]int{"count": 3})
	b.Emit("loopagent:updated", nil)

	for range 2 {
		select {
		case got := <-resultCh:
			switch got.eventType {
			case "issues:updated":
				if !strings.Contains(got.data, "count") {
					t.Fatalf("unexpected issues data: %s", got.data)
				}
			case "loopagent:updated":
				// nil marshals to "null"
			default:
				t.Fatalf("unexpected event type: %s", got.eventType)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for SSE event")
		}
	}
}

func TestBroker_ServeHTTP(t *testing.T) {
	b := sse.New()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events/{eventName}", b.ServeHTTP)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Start SSE request in background.
	resultCh := make(chan string, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/api/events/my.event")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				resultCh <- data
				return
			}
		}
	}()

	// Give the goroutine time to connect.
	time.Sleep(50 * time.Millisecond)
	b.Emit("my.event", map[string]string{"hello": "world"})

	select {
	case msg := <-resultCh:
		if !strings.Contains(msg, "hello") {
			t.Fatalf("unexpected SSE data: %s", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for SSE event")
	}
}
